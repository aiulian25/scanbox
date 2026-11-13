// Package web serves the Scan Box page: Scanner, Settings and sign-in.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"scanbox/internal/paperless"
	"scanbox/internal/scanner"
	"scanbox/internal/settings"
	"scanbox/internal/setup"
	"scanbox/internal/station"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static
var staticFiles embed.FS

const (
	checkTimeout       = 15 * time.Second
	heartbeatInterval  = 20 * time.Second
	formLimitBytes     = 64 * 1024
	moveEarlier        = "earlier"
	moveLater          = "later"
	versionSeparator   = "."
	signInPath         = "/signin"
	lockedPage         = "locked"
	finishedImageCache = "private, max-age=86400"
	noCache            = "no-store"
	// contentSecurityPolicy lets a page load only Scan Box's own files, and no other site
	// frame it, change its base or receive its forms. The pages have no inline script or style.
	contentSecurityPolicy = "default-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"
	// referrerPolicy keeps page addresses from other sites. It is not no-referrer: with
	// that, browsers send "Origin: null" on the page's own changes, which sameOrigin refuses.
	referrerPolicy = "same-origin"
	healthy        = "ok"
)

// Checker proves the Paperless address and token work, and asks Paperless what it runs.
type Checker interface {
	Check(ctx context.Context) error
	Probe(ctx context.Context) (paperless.ServerInfo, error)
}

type Options struct {
	DataDir string
	// Version is the release the program was built as, or dev.
	Version  string
	Password string
	// Access says who may use the page. Left unset, nobody may.
	Access setup.Access
	// Lock says why a locked page is locked, and what to do about it.
	Lock setup.Check
	// Hosts are the names with a dot in them that the page answers to, besides addresses
	// and local names.
	Hosts []string
	// TrustedProxies are the reverse proxies whose X-Forwarded-For and X-Forwarded-Proto
	// are believed.
	TrustedProxies []netip.Prefix
	Station        *station.Station
	Settings       *settings.Store
	Checker        Checker
	// Facts are what the container provides; the scanner's name is filled in live.
	Facts setup.Facts
	Now   func() time.Time
}

type Server struct {
	options  Options
	guard    *guard
	pages    map[string]*template.Template
	build    build
	probeNow chan struct{}

	mutex        sync.Mutex
	tokenProblem string
	remote       remoteState
}

func New(options Options) (*Server, error) {
	if options.Now == nil {
		options.Now = time.Now
	}
	guard, err := newGuard(options.DataDir, options.Password, options.Access, options.TrustedProxies, options.Now)
	if err != nil {
		return nil, err
	}
	pages, err := parsePages()
	if err != nil {
		return nil, err
	}
	return &Server{options: options, guard: guard, pages: pages, build: readBuild(), probeNow: make(chan struct{}, 1)}, nil
}

func parsePages() (map[string]*template.Template, error) {
	functions := template.FuncMap{
		"actionLabel":    func(action settings.Action) string { return actionLabels[action] },
		"resolutionNote": func(resolution int) string { return resolutionNotes[resolution] },
		"paperLabel":     func(size settings.PaperSize) string { return paperLabels[size] },
		"plural":         countWords,
		"keepLabel":      func(days int) string { return keepLabels[days] },
		"reserveLabel":   func(megabytes int) string { return reserveLabels[megabytes] },
		"headline":       headline,
	}
	pages := map[string]*template.Template{}
	for _, page := range []string{"scanner", "scans", "settings", "signin", lockedPage} {
		parsed, err := template.New("").Funcs(functions).ParseFS(templateFiles, "templates/layout.html", "templates/icons.html", "templates/live.html", "templates/"+page+".html")
		if err != nil {
			return nil, err
		}
		pages[page] = parsed
	}
	return pages, nil
}

// CheckPaperless runs the token check once, so the setup checker knows whether Paperless accepts it.
func (s *Server) CheckPaperless(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	err := s.options.Checker.Check(ctx)
	problem := ""
	if errors.Is(err, paperless.ErrTokenRejected) || errors.Is(err, paperless.ErrNotAllowed) {
		problem = err.Error()
	}
	s.mutex.Lock()
	s.tokenProblem = problem
	s.mutex.Unlock()
	return err
}

func (s *Server) Handler() http.Handler {
	routes := http.NewServeMux()
	static, _ := fs.Sub(staticFiles, "static")
	routes.Handle("GET /static/", http.StripPrefix("/static/", filesOnly(http.FileServerFS(static))))
	routes.HandleFunc("GET /healthz", s.health)
	if s.guard.locked() {
		routes.HandleFunc("/", s.lockedNotice)
		return securityHeaders(s.knownHostsOnly(routes))
	}
	routes.HandleFunc("GET "+signInPath, s.signInPage)
	routes.HandleFunc("POST "+signInPath, s.signIn)

	protected := http.NewServeMux()
	protected.HandleFunc("POST /signout", s.signOut)
	protected.HandleFunc("GET /{$}", s.scannerPage)
	protected.HandleFunc("GET /settings", s.settingsPage)
	protected.HandleFunc("GET /scans", s.scansPage)
	protected.HandleFunc("GET /documents/{id}/download", s.download)
	protected.HandleFunc("GET /live", s.live)
	protected.HandleFunc("GET /documents/{file}", s.documentPDF)
	protected.HandleFunc("GET /documents/{id}/pages/{page}", s.pageImage(func(request *http.Request, page int) (string, error) {
		return s.options.Station.ThumbnailPath(request.PathValue("id"), page)
	}, finishedImageCache))
	protected.HandleFunc("GET /documents/{id}/previews/{page}", s.pageImage(func(request *http.Request, page int) (string, error) {
		return s.options.Station.PreviewPath(request.PathValue("id"), page)
	}, finishedImageCache))
	protected.HandleFunc("GET /open/pages/{page}", s.pageImage(func(_ *http.Request, page int) (string, error) {
		return s.options.Station.OpenThumbnailPath(page)
	}, noCache))
	protected.HandleFunc("GET /open/previews/{page}", s.pageImage(func(_ *http.Request, page int) (string, error) {
		return s.options.Station.OpenPreviewPath(page)
	}, noCache))
	protected.HandleFunc("POST /api/action", s.action)
	protected.HandleFunc("POST /api/remove-last", s.removeLast)
	protected.HandleFunc("POST /api/open/pages/{number}/move", s.movePage)
	protected.HandleFunc("POST /api/documents/{id}/name", s.rename)
	protected.HandleFunc("POST /api/documents/{id}/send", s.send)
	protected.HandleFunc("POST /api/documents/{id}/delete", s.delete)
	protected.HandleFunc("POST /api/documents/{id}/retry", s.retry)
	protected.HandleFunc("POST /api/send-waiting", s.sendWaiting)
	protected.HandleFunc("POST /api/settings", s.changeSetting)
	protected.HandleFunc("POST /api/paperless/test", s.testPaperless)
	protected.HandleFunc("GET /api/date-time", s.dateTime)
	protected.HandleFunc("GET /api/status", s.status)
	routes.Handle("/", s.requireSignIn(protected))
	return securityHeaders(s.knownHostsOnly(routes))
}

// filesOnly serves files and never lists a folder.
func filesOnly(files http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "" || strings.HasSuffix(request.URL.Path, "/") {
			http.NotFound(writer, request)
			return
		}
		files.ServeHTTP(writer, request)
	})
}

// securityHeaders marks every answer as being what its type says, and keeps the address
// of a page from going to other sites. Pages add their content policy when written.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", referrerPolicy)
		next.ServeHTTP(writer, request)
	})
}

// requireSignIn lets only signed-in visitors in, takes changes only from this page, and
// reads no more of a change than the largest form the page sends.
func (s *Server) requireSignIn(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && !sameOrigin(request) {
			http.Error(writer, "This change came from another site and was refused.", http.StatusForbidden)
			return
		}
		if request.Method == http.MethodPost && request.ContentLength > formLimitBytes {
			http.Error(writer, "This change is larger than any the page sends, and was refused.", http.StatusRequestEntityTooLarge)
			return
		}
		request.Body = http.MaxBytesReader(writer, request.Body, formLimitBytes)
		if s.guard.signedIn(request) {
			next.ServeHTTP(writer, request)
			return
		}
		if request.Method == http.MethodGet && !strings.HasPrefix(request.URL.Path, "/api/") {
			http.Redirect(writer, request, signInPath, http.StatusSeeOther)
			return
		}
		http.Error(writer, "Please sign in again.", http.StatusUnauthorized)
	})
}

// ---- page data ----

type pageData struct {
	Title       string
	Page        string
	SignedIn    bool
	PasswordSet bool
	Settings    settings.Settings
	live
	Actions        []settings.Action
	Buttons        []buttonView
	Checks         []setup.Check
	FullCompose    string
	TokenSet       bool
	Lock           setup.Check
	Error          string
	KeepChoices    []int
	ReserveChoices []int
	Usage          string
	Search         searchView
	DateTime       dateTimeView
	BuildID        string
	BuildNote      string
	// Release names the running release for the header: v0.1.0, or dev for other builds.
	Release     string
	ShowArchive bool
	// SettingsDamaged says the saved settings couldn't be read and the defaults are in use.
	SettingsDamaged string
}

// live is the part of every page that changes while it is open: the status in the
// header, the scanner and its scans, the archive and the line at the foot.
type live struct {
	Status       statusView
	Device       scanner.Details
	Paperless    paperlessView
	Missing      int
	FirstMissing string
	HasScanner   bool
	Colour       bool
	Resolutions  []int
	PaperSizes   []settings.PaperSize
	OpenPages    []openPageView
	// PageVersion changes whenever a page of the open document is added, removed or
	// moved, so its pictures, which are named by place, are fetched again.
	PageVersion string
	FinishesAt  string
	Review      []documentView
	ReviewKey   string
	CanSend     bool
	Recent      []documentView
	Waiting     int
	Archive     archiveView
}

func (s *Server) facts(status station.Status) setup.Facts {
	facts := s.options.Facts
	facts.ScannerName = status.Scanner
	facts.TasksDenied = errors.Is(status.ConfirmError, paperless.ErrNoTaskAccess)
	s.mutex.Lock()
	facts.TokenProblem = s.tokenProblem
	s.mutex.Unlock()
	return facts
}

// liveData gathers what changes, from the station's status; chosenMonth and chosenDay are
// the archive's choice on the Scans page.
func (s *Server) liveData(status station.Status, chosenMonth, chosenDay string) live {
	current := s.options.Settings.Get()
	now := s.options.Now()
	checks := setup.Checks(s.facts(status))
	s.mutex.Lock()
	remote := s.remote
	s.mutex.Unlock()
	data := live{
		Status:      describeStatus(status, current),
		Device:      status.Device,
		Paperless:   describePaperless(remote, current.PaperlessURL, current.Clock()),
		Missing:     setup.MissingCount(checks),
		HasScanner:  status.Scanner != "",
		Colour:      current.Colour,
		Resolutions: settings.Resolutions,
		PaperSizes:  settings.PaperSizes,
		CanSend:     s.canSend(current),
		Waiting:     len(status.Waiting),
		Archive:     s.archive(chosenMonth, chosenDay),
	}
	for _, check := range checks {
		if !check.OK && data.FirstMissing == "" {
			data.FirstMissing = strings.ToLower(check.Title)
		}
	}
	data.OpenPages, data.PageVersion = describeOpenPages(status)
	if !status.FinishesAt.IsZero() {
		data.FinishesAt = current.Clock().Time(status.FinishesAt)
	}
	keys := make([]string, 0, len(status.Review))
	for _, document := range status.Review {
		data.Review = append(data.Review, describeDocument(document, current.PaperlessURL, data.CanSend, now, current.Clock()))
		keys = append(keys, document.ID+"="+document.Name)
	}
	data.ReviewKey = strings.Join(keys, "|")
	for _, group := range [][]station.Document{status.Failed, status.Waiting, status.Submitted, status.Sent} {
		for _, document := range group {
			data.Recent = append(data.Recent, describeDocument(document, current.PaperlessURL, data.CanSend, now, current.Clock()))
		}
	}
	return data
}

// openPageView is a page of the open document: its place, which names its pictures, and
// its number, which the buttons that move it name.
type openPageView struct {
	Place, Number int
	First, Last   bool
}

// describeOpenPages lists the open document's pages in order, with a version made of when
// the newest page came and the order, which changes with every change to the pages.
func describeOpenPages(status station.Status) ([]openPageView, string) {
	pages := make([]openPageView, 0, len(status.OpenPageNumbers))
	numbers := make([]string, 0, len(status.OpenPageNumbers))
	for index, number := range status.OpenPageNumbers {
		pages = append(pages, openPageView{Place: index + 1, Number: number, First: index == 0, Last: index == len(status.OpenPageNumbers)-1})
		numbers = append(numbers, strconv.Itoa(number))
	}
	return pages, strconv.FormatInt(status.FinishesAt.Unix(), 10) + versionSeparator + strings.Join(numbers, versionSeparator)
}

// canSend says whether scans can go to Paperless: its address and the token are set.
func (s *Server) canSend(current settings.Settings) bool {
	return current.PaperlessURL != "" && s.options.Facts.TokenSource != setup.Missing
}

func (s *Server) page(request *http.Request, title, page string) pageData {
	current := s.options.Settings.Get()
	status := s.options.Station.Status()
	query := request.URL.Query()
	data := pageData{
		Title: title, Page: page, SignedIn: s.guard.signedIn(request), PasswordSet: s.guard.enabled(),
		Settings: current, live: s.liveData(status, query.Get("month"), query.Get("day")),
		Actions: settings.Actions, Buttons: buttonViews(current), Checks: setup.Checks(s.facts(status)),
		FullCompose: setup.FullCompose(), TokenSet: s.options.Facts.TokenSource != setup.Missing,
		BuildID: s.build.ID, Release: releaseLabel(s.options.Version), ShowArchive: page != "settings",
	}
	if !s.build.Built.IsZero() {
		data.BuildNote = "Built " + current.Clock().DateTime(s.build.Built) + " with " + s.build.Go
	}
	return data
}

func (s *Server) render(writer http.ResponseWriter, page string, data pageData) {
	s.renderAs(writer, http.StatusOK, page, data)
}

// renderAs writes a page under the given status. The headers go out with the status, so
// they are set first.
func (s *Server) renderAs(writer http.ResponseWriter, status int, page string, data pageData) {
	var output bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&output, "layout", data); err != nil {
		log.Printf("page %s: %v", page, err)
		http.Error(writer, "This page couldn't be shown.", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Security-Policy", contentSecurityPolicy)
	writer.WriteHeader(status)
	writer.Write(output.Bytes())
}

// lockedNotice is all a locked page serves, whatever was asked for: why it is locked, and
// what to do to open it.
func (s *Server) lockedNotice(writer http.ResponseWriter, _ *http.Request) {
	s.renderAs(writer, http.StatusForbidden, lockedPage, pageData{Title: "Locked", Page: lockedPage, Lock: s.options.Lock})
}

// ---- pages ----

func (s *Server) scannerPage(writer http.ResponseWriter, request *http.Request) {
	s.render(writer, "scanner", s.page(request, "Scanner", "scanner"))
}

func (s *Server) settingsPage(writer http.ResponseWriter, request *http.Request) {
	data := s.page(request, "Settings", "settings")
	data.KeepChoices = settings.KeepChoices
	data.ReserveChoices = settings.ReserveChoices
	data.Usage = s.usage()
	data.DateTime = describeDateTime(data.Settings, s.options.Now())
	data.SettingsDamaged = sentence(s.options.Settings.Damaged)
	s.render(writer, "settings", data)
}

// dateTime gives the Date and time section again after a change, so its examples follow.
func (s *Server) dateTime(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(describeDateTime(s.options.Settings.Get(), s.options.Now()))
}

func (s *Server) usage() string {
	used, free := s.options.Station.Usage()
	return fmt.Sprintf("Scans use %s. %s is free on this box.", readableSize(used), readableSize(free))
}

func (s *Server) scansPage(writer http.ResponseWriter, request *http.Request) {
	data := s.page(request, "Scans", "scans")
	data.Search = s.search(request)
	data.Usage = s.usage()
	s.render(writer, "scans", data)
}

// download hands the PDF over as a file named after the scan, whatever its state.
func (s *Server) download(writer http.ResponseWriter, request *http.Request) {
	document, err := s.options.Station.Document(request.PathValue("id"))
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	path, err := s.options.Station.PDFPath(document.ID)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "application/pdf")
	writer.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": document.Name + ".pdf"}))
	http.ServeFile(writer, request, path)
}

func (s *Server) signInPage(writer http.ResponseWriter, request *http.Request) {
	if s.guard.signedIn(request) {
		http.Redirect(writer, request, "/", http.StatusSeeOther)
		return
	}
	s.render(writer, "signin", pageData{Title: "Sign in", Page: "signin"})
}

func (s *Server) signIn(writer http.ResponseWriter, request *http.Request) {
	if !sameOrigin(request) {
		http.Error(writer, "This sign-in came from another site and was refused.", http.StatusForbidden)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, formLimitBytes)
	accepted, err := s.guard.signIn(writer, request, request.FormValue("password"))
	if accepted {
		http.Redirect(writer, request, "/", http.StatusSeeOther)
		return
	}
	message := "That password isn't right."
	if err != nil {
		message = "Too many wrong passwords. Wait a minute and try again."
	}
	s.renderAs(writer, http.StatusUnauthorized, "signin", pageData{Title: "Sign in", Page: "signin", Error: message})
}

// signOut ends every session, on every device. If the count can't be written down, the
// sessions end anyway until the next start, and the log says so.
func (s *Server) signOut(writer http.ResponseWriter, request *http.Request) {
	outcome := "every session ended"
	if err := s.guard.signOut(writer); err != nil {
		outcome = fmt.Sprintf("every session ended, but only until the next start: %v", err)
	}
	log.Printf("sign-out from %s: %s", s.guard.clientAddress(request), outcome)
	http.Redirect(writer, request, signInPath, http.StatusSeeOther)
}

// health answers Docker's health check without sign-in, so it says no more than "ok": both
// of the station's loops are moving. Otherwise it answers 503 with the reason.
func (s *Server) health(writer http.ResponseWriter, _ *http.Request) {
	if err := s.options.Station.Health(); err != nil {
		http.Error(writer, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writer.Write([]byte(healthy + "\n"))
}

// status gives someone signed in what health leaves out.
func (s *Server) status(writer http.ResponseWriter, _ *http.Request) {
	status := s.options.Station.Status()
	health := healthy
	if err := s.options.Station.Health(); err != nil {
		health = err.Error()
	}
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(map[string]any{
		"scanner": status.Scanner, "to_review": len(status.Review), "waiting": len(status.Waiting),
		"accepted": len(status.Submitted), "failed": len(status.Failed), "health": health,
	})
}

// ---- live updates ----

var liveParts = []string{"badge", "setup-notice", "controls", "glass", "review", "recent", "archive-count", "archive", "footer"}

// reviewKeyPart goes with every live update: the page compares it with the review list it
// has before redrawing that list, which may hold a name being typed.
const reviewKeyPart = "reviewKey"

func (s *Server) liveFragments(chosenMonth, chosenDay string) (map[string]string, error) {
	data := pageData{live: s.liveData(s.options.Station.Status(), chosenMonth, chosenDay), Settings: s.options.Settings.Get()}
	parts := map[string]string{reviewKeyPart: data.ReviewKey}
	for _, name := range liveParts {
		var output bytes.Buffer
		if err := s.pages["scanner"].ExecuteTemplate(&output, name, data); err != nil {
			return nil, err
		}
		parts[name] = output.String()
	}
	return parts, nil
}

// partsChanged gives the parts that differ from those the page shows, and notes them as
// shown.
func partsChanged(parts, shown map[string]string) map[string]string {
	changed := map[string]string{}
	for name, content := range parts {
		if previous, sent := shown[name]; sent && previous == content {
			continue
		}
		changed[name] = content
		shown[name] = content
	}
	return changed
}

// live streams the parts of a page that changed, after every change. The page passes on
// its own month or day, so its archive stays on the month it shows.
func (s *Server) live(writer http.ResponseWriter, request *http.Request) {
	flusher, canStream := writer.(http.Flusher)
	if !canStream {
		http.Error(writer, "Live updates aren't supported here.", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-store")
	changes, stop := s.options.Station.Watch()
	defer stop()
	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()
	query := request.URL.Query()
	shown := map[string]string{}
	send := func() bool {
		parts, err := s.liveFragments(query.Get("month"), query.Get("day"))
		if err != nil {
			log.Printf("live: %v", err)
			return false
		}
		changed := partsChanged(parts, shown)
		if len(changed) == 0 {
			return true
		}
		changed[reviewKeyPart] = parts[reviewKeyPart]
		encoded, _ := json.Marshal(changed)
		_, err = fmt.Fprintf(writer, "data: %s\n\n", encoded)
		flusher.Flush()
		return err == nil
	}
	if !send() {
		return
	}
	for {
		select {
		case <-request.Context().Done():
			return
		case <-changes:
			if !send() {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(writer, ": still here\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---- files ----

func (s *Server) documentPDF(writer http.ResponseWriter, request *http.Request) {
	id, isPDF := strings.CutSuffix(request.PathValue("file"), ".pdf")
	if !isPDF {
		http.NotFound(writer, request)
		return
	}
	path, err := s.options.Station.PDFPath(id)
	if err != nil {
		http.NotFound(writer, request)
		return
	}
	writer.Header().Set("Content-Type", "application/pdf")
	writer.Header().Set("Content-Disposition", "inline")
	http.ServeFile(writer, request, path)
}

// pageImage serves one page picture, found by page number. A finished document's
// pictures never change, so browsers may keep them; the open document's change.
func (s *Server) pageImage(find func(*http.Request, int) (string, error), cache string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		page, err := pageNumber(request)
		if err != nil {
			http.NotFound(writer, request)
			return
		}
		path, err := find(request, page)
		if err != nil {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Cache-Control", cache)
		http.ServeFile(writer, request, path)
	}
}

func pageNumber(request *http.Request) (int, error) {
	number, isImage := strings.CutSuffix(request.PathValue("page"), ".jpg")
	if !isImage {
		return 0, errors.New("not a page image")
	}
	return strconv.Atoi(number)
}

// ---- actions ----

func (s *Server) answer(writer http.ResponseWriter, err error) {
	writer.Header().Set("Content-Type", "application/json")
	if err == nil {
		writer.Write([]byte(`{"ok":true}`))
		return
	}
	status := http.StatusBadRequest
	if errors.Is(err, station.ErrNotFound) {
		status = http.StatusNotFound
	}
	if errors.Is(err, station.ErrBusy) || errors.Is(err, station.ErrSending) {
		status = http.StatusConflict
	}
	writer.WriteHeader(status)
	json.NewEncoder(writer).Encode(map[string]any{"ok": false, "message": plainMessage(err)})
}

// plainMessage drops the "invalid setting:" prefix so people see only the advice.
func plainMessage(err error) string {
	_, advice, found := strings.Cut(err.Error(), settings.ErrInvalid.Error()+": ")
	if found {
		return advice
	}
	return err.Error()
}

func (s *Server) action(writer http.ResponseWriter, request *http.Request) {
	action := settings.Action(request.FormValue("action"))
	if _, known := actionLabels[action]; !known {
		s.answer(writer, fmt.Errorf("unknown action"))
		return
	}
	s.answer(writer, s.options.Station.Perform(action))
}

func (s *Server) removeLast(writer http.ResponseWriter, _ *http.Request) {
	s.answer(writer, s.options.Station.RemoveLastPage())
}

// moveOffsets are the directions a page of the open document can move, one place each.
var moveOffsets = map[string]int{moveEarlier: -1, moveLater: 1}

func (s *Server) movePage(writer http.ResponseWriter, request *http.Request) {
	number, err := strconv.Atoi(request.PathValue("number"))
	offset, known := moveOffsets[request.FormValue("direction")]
	if err != nil || !known {
		s.answer(writer, station.ErrCantMove)
		return
	}
	s.answer(writer, s.options.Station.MoveOpenPage(number, offset))
}

func (s *Server) rename(writer http.ResponseWriter, request *http.Request) {
	s.answer(writer, s.options.Station.Rename(request.PathValue("id"), strings.TrimSpace(request.FormValue("name"))))
}

func (s *Server) send(writer http.ResponseWriter, request *http.Request) {
	s.answer(writer, s.options.Station.Send(request.PathValue("id")))
}

func (s *Server) delete(writer http.ResponseWriter, request *http.Request) {
	s.answer(writer, s.options.Station.Delete(request.PathValue("id")))
}

func (s *Server) retry(writer http.ResponseWriter, request *http.Request) {
	s.answer(writer, s.options.Station.Retry(request.PathValue("id")))
}

// sendWaiting is Send now: every waiting scan goes at once, whatever pause it was in.
func (s *Server) sendWaiting(writer http.ResponseWriter, _ *http.Request) {
	s.options.Station.RetryNow()
	s.answer(writer, nil)
}

func (s *Server) changeSetting(writer http.ResponseWriter, request *http.Request) {
	field := request.FormValue("field")
	if _, err := s.options.Settings.Set(field, request.FormValue("value")); err != nil {
		s.answer(writer, err)
		return
	}
	log.Printf("settings: %s changed", field)
	if field == "paperless_url" {
		go s.CheckPaperless(context.Background())
		s.askPaperlessNow()
		s.options.Station.RetryNow()
	}
	s.options.Station.Touch()
	s.answer(writer, nil)
}

func (s *Server) testPaperless(writer http.ResponseWriter, request *http.Request) {
	err := s.CheckPaperless(request.Context())
	s.options.Station.Touch()
	writer.Header().Set("Content-Type", "application/json")
	if err != nil {
		json.NewEncoder(writer).Encode(map[string]any{"ok": false, "message": firstClause(err.Error()) + "."})
		return
	}
	json.NewEncoder(writer).Encode(map[string]any{"ok": true, "message": "Connected. Paperless accepts the token."})
}

func urlQuery(text string) string { return url.QueryEscape(text) }
