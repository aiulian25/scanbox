package web

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"scanbox/internal/clock"
	"scanbox/internal/paperless"
	"scanbox/internal/scanner"
	"scanbox/internal/settings"
	"scanbox/internal/setup"
	"scanbox/internal/station"
)

const (
	testPassword     = "correct horse"
	paperlessAddress = "https://paperless.example.com"
	otherAddress     = "https://elsewhere.example.com"
	foreignHost      = "evil.example:28761"
	listedHost       = "scan.example.org"
	lockedTitle      = "This page is locked"
	waitLimit        = 30 * time.Second
)

type nobody struct{}

func (nobody) Upload(context.Context, string, string, string, time.Time) (string, error) {
	return "task", nil
}
func (nobody) Check(context.Context) error { return nil }

// storedDocumentID is the document nobody says it stored for every upload.
const storedDocumentID = 4321

func (nobody) Confirm(context.Context, string) (station.Delivery, error) {
	return station.Delivery{DocumentID: storedDocumentID, Listed: true}, nil
}

func (nobody) Find(context.Context, string) (string, error) { return "", nil }
func (nobody) Probe(context.Context) (paperless.ServerInfo, error) {
	return paperless.ServerInfo{Version: "3.0.0", APIVersion: "10"}, nil
}

type site struct {
	server  *httptest.Server
	client  *http.Client
	station *station.Station
	page    *Server
}

func newSite(t *testing.T, password string) *site {
	t.Helper()
	return newSiteWith(t, password, nobody{})
}

// newSiteWith serves a page that asks for the password or, without one, is open to everyone.
func newSiteWith(t *testing.T, password string, checker Checker) *site {
	t.Helper()
	access := setup.Open
	if password != "" {
		access = setup.BySignIn
	}
	return newSiteFor(t, Options{Password: password, Access: access, Checker: checker})
}

// newSiteFor puts a data folder, settings and a station with a pretend scanner behind the
// given options. A checker that can also take documents is where the station sends them.
func newSiteFor(t *testing.T, options Options) *site {
	t.Helper()
	directory := options.DataDir
	if directory == "" {
		directory = t.TempDir()
	}
	store, err := settings.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	store.Set("resolution_dpi", "150")
	store.Set("paperless_url", paperlessAddress)
	destination, takesDocuments := options.Checker.(station.Destination)
	if !takesDocuments {
		destination = nobody{}
	}
	box, err := station.New(station.Config{DataDir: directory, Settings: store, Driver: scanner.NewFake(), Destination: destination,
		ButtonPoll: 5 * time.Millisecond, Retry: 50 * time.Millisecond, Reopen: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// The station is waited for when the test ends, so no write of its outlives the files.
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); box.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-stopped })
	deadline := time.Now().Add(waitLimit)
	for box.Status().Scanner == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	passwordSource := setup.Missing
	if options.Password != "" {
		passwordSource = setup.FromFile
	}
	options.DataDir, options.Station, options.Settings = directory, box, store
	options.Facts = setup.Facts{TokenSource: setup.FromFile, PasswordSource: passwordSource, DataIsVolume: true}
	page, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(page.Handler())
	t.Cleanup(server.Close)
	jar, _ := cookiejar.New(nil)
	// The limit makes a stream that should have been refused a failure instead of a test that never ends.
	return &site{server: server, client: &http.Client{Jar: jar, Timeout: waitLimit}, station: box, page: page}
}

// ownHost is the address the test server is reached at, as a browser would name it.
func (s *site) ownHost() string { return strings.TrimPrefix(s.server.URL, "http://") }

func (s *site) post(t *testing.T, path string, values url.Values) (*http.Response, string) {
	t.Helper()
	return s.postAs(t, s.ownHost(), path, values)
}

// postAs sends a change the way a browser showing the page under the given name would:
// to that name, and with that name as where the change comes from.
func (s *site) postAs(t *testing.T, host, path string, values url.Values) (*http.Response, string) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodPost, s.server.URL+path, strings.NewReader(values.Encode()))
	request.Host = host
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "http://"+host)
	return s.do(t, request)
}

func (s *site) get(t *testing.T, path string) (*http.Response, string) {
	t.Helper()
	return s.getAs(t, s.ownHost(), path)
}

func (s *site) getAs(t *testing.T, host, path string) (*http.Response, string) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, s.server.URL+path, nil)
	request.Host = host
	return s.do(t, request)
}

func (s *site) do(t *testing.T, request *http.Request) (*http.Response, string) {
	t.Helper()
	response, err := s.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	return response, string(body)
}

func TestSignInProtectsThePage(t *testing.T) {
	s := newSite(t, testPassword)
	response, body := s.get(t, "/")
	if response.Request.URL.Path != signInPath || !strings.Contains(body, `type="password"`) {
		t.Fatalf("not sent to sign in: %s", response.Request.URL.Path)
	}
	if response, _ := s.post(t, "/api/action", url.Values{"action": {"one_grey_page"}}); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an action without signing in gave %d", response.StatusCode)
	}
	response, body = s.post(t, signInPath, url.Values{"password": {"wrong"}})
	if response.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "That password isn&#39;t right.") {
		t.Fatalf("wrong password: %d", response.StatusCode)
	}
	response, body = s.post(t, signInPath, url.Values{"password": {testPassword}})
	if response.Request.URL.Path != "/" || !strings.Contains(body, "Recent scans") && !strings.Contains(body, "Single page scan") {
		t.Fatalf("after signing in: %s", response.Request.URL.Path)
	}
	if strings.Contains(body, testPassword) {
		t.Fatal("the page shows the password")
	}
	response, _ = s.post(t, "/signout", url.Values{})
	if response.Request.URL.Path != signInPath {
		t.Fatal("signing out did not go back to the sign-in page")
	}
}

func TestEveryAnswerCarriesTheSecurityHeaders(t *testing.T) {
	s := newSite(t, "")
	s.post(t, "/api/action", url.Values{"action": {"one_grey_page"}})
	document := waitForReview(t, s)
	pages := []string{"/", "/scans", "/settings"}
	files := []string{"/static/app.css", "/documents/" + document.ID + ".pdf", "/documents/" + document.ID + "/previews/1.jpg", "/healthz"}
	for _, path := range append(pages, files...) {
		response, _ := s.get(t, path)
		if response.Header.Get("X-Content-Type-Options") != "nosniff" || response.Header.Get("Referrer-Policy") != referrerPolicy {
			t.Errorf("%s: %v", path, response.Header)
		}
	}
	for _, path := range pages {
		if response, _ := s.get(t, path); response.Header.Get("Content-Security-Policy") != contentSecurityPolicy {
			t.Errorf("the page %s has no content policy", path)
		}
	}
	if response, _ := s.get(t, "/documents/"+document.ID+".pdf"); response.Header.Get("Content-Security-Policy") != "" {
		t.Error("a PDF gets no page's content policy, which could stop the browser's own viewer")
	}
	if response, _ := s.getAs(t, foreignHost, "/"); response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("a refused name's answer lacks nosniff")
	}
}

func TestSignOutEndsEveryCopyOfTheSession(t *testing.T) {
	s := newSite(t, testPassword)
	s.post(t, signInPath, url.Values{"password": {testPassword}})
	address, _ := url.Parse(s.server.URL)
	copied, _ := cookiejar.New(nil)
	copied.SetCookies(address, s.client.Jar.Cookies(address))
	thief := &http.Client{Jar: copied, Timeout: waitLimit}
	if response, err := thief.Get(s.server.URL + "/"); err != nil || response.Request.URL.Path != "/" {
		t.Fatalf("a copy of the cookie should work before signing out: %v", err)
	}
	s.post(t, "/signout", url.Values{})
	if response, err := thief.Get(s.server.URL + "/"); err != nil || response.Request.URL.Path != signInPath {
		t.Fatalf("after signing out, the copy still opens the page: %v", err)
	}
	restarted, err := newGuard(s.page.options.DataDir, testPassword, setup.BySignIn, nil, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, cookie := range copied.Cookies(address) {
		request.AddCookie(cookie)
	}
	if restarted.signedIn(request) {
		t.Fatal("after a restart, the copy works again")
	}
	s.post(t, signInPath, url.Values{"password": {testPassword}})
	if response, _ := s.get(t, "/"); response.Request.URL.Path != "/" {
		t.Fatal("signing in again after signing out doesn't work")
	}
}

func TestOverHTTPSTheCookieIsSecure(t *testing.T) {
	s := newSiteFor(t, Options{Password: testPassword, Access: setup.BySignIn, Checker: nobody{}, TrustedProxies: []netip.Prefix{netip.MustParsePrefix("172.18.0.2/32")}})
	secure := httptest.NewTLSServer(s.page.Handler())
	defer secure.Close()
	request, _ := http.NewRequest(http.MethodPost, secure.URL+signInPath, strings.NewReader(url.Values{"password": {testPassword}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", secure.URL)
	client := secure.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil || len(response.Cookies()) != 1 || !response.Cookies()[0].Secure {
		t.Fatalf("signing in over https: %v %v", err, response.Cookies())
	}
	for name, from := range map[string]struct {
		remote string
		secure bool
	}{"the trusted proxy": {"172.18.0.2:41000", true}, "anyone else": {"192.168.10.30:41000", false}} {
		proxied := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:28761"+signInPath, strings.NewReader(url.Values{"password": {testPassword}}.Encode()))
		proxied.RemoteAddr = from.remote
		proxied.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		proxied.Header.Set("Origin", "http://127.0.0.1:28761")
		proxied.Header.Set(forwardedProto, https)
		recorder := httptest.NewRecorder()
		s.page.Handler().ServeHTTP(recorder, proxied)
		cookies := recorder.Result().Cookies()
		if len(cookies) != 1 || cookies[0].Secure != from.secure {
			t.Errorf("X-Forwarded-Proto: https from %s: %v", name, cookies)
		}
	}
}

func TestBehindTheProxyALockoutAppliesToOneClient(t *testing.T) {
	proxy := netip.MustParsePrefix("172.18.0.2/32")
	g, err := newGuard(t.TempDir(), testPassword, setup.BySignIn, []netip.Prefix{proxy}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	from := func(remote, forwarded string) *http.Request {
		request := httptest.NewRequest(http.MethodPost, signInPath, nil)
		request.RemoteAddr = remote
		if forwarded != "" {
			request.Header.Set(forwardedFor, forwarded)
		}
		return request
	}
	cases := map[string]struct {
		request *http.Request
		want    string
	}{
		"through the proxy":                 {from("172.18.0.2:5000", "203.0.113.9, 192.168.10.30"), "192.168.10.30"},
		"through the proxy, IPv4 in IPv6":   {from("[::ffff:172.18.0.2]:5000", "192.168.10.31"), "192.168.10.31"},
		"the proxy, saying nothing":         {from("172.18.0.2:5000", ""), "172.18.0.2"},
		"the proxy, saying nonsense":        {from("172.18.0.2:5000", "nonsense"), "172.18.0.2"},
		"anyone else, claiming to be other": {from("192.168.10.40:5000", "192.168.10.30"), "192.168.10.40"},
	}
	for name, example := range cases {
		if got := g.clientAddress(example.request); got != example.want {
			t.Errorf("%s: %s, want %s", name, got, example.want)
		}
	}
	for range failuresAllowed {
		g.recordFailure(g.clientAddress(from("172.18.0.2:5000", "192.168.10.30")))
	}
	if !g.lockedOut(g.clientAddress(from("172.18.0.2:5000", "192.168.10.30"))) || g.lockedOut(g.clientAddress(from("172.18.0.2:5001", "192.168.10.31"))) {
		t.Fatal("five wrong passwords from one client behind the proxy should lock out that client only")
	}
}

func TestTooManyWrongPasswordsLockOut(t *testing.T) {
	if testing.Short() {
		t.Skip("takes five seconds")
	}
	s := newSite(t, testPassword)
	for attempt := 0; attempt < failuresAllowed; attempt++ {
		s.post(t, signInPath, url.Values{"password": {"wrong"}})
	}
	_, body := s.post(t, signInPath, url.Values{"password": {testPassword}})
	if !strings.Contains(body, "Too many wrong passwords") {
		t.Fatal("the right password was accepted during a lock-out")
	}
}

func TestChangesFromOtherSitesAreRefused(t *testing.T) {
	s := newSite(t, "")
	request, _ := http.NewRequest(http.MethodPost, s.server.URL+"/api/action", strings.NewReader("action=one_grey_page"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", "https://evil.example")
	response, err := s.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("a foreign change gave %d", response.StatusCode)
	}
}

func TestALockedPageServesOnlyItsNotice(t *testing.T) {
	t.Setenv(setup.AllowNoPasswordVariable, "")
	access, lock := setup.PageAccess(setup.Missing, nil)
	s := newSiteFor(t, Options{Access: access, Lock: lock, Checker: nobody{}})
	pages := []string{"/", "/settings", "/scans", "/live", signInPath, "/documents/any.pdf", "/documents/any/download", "/open/pages/1.jpg", "/api/date-time", "/nowhere"}
	for _, path := range pages {
		response, body := s.get(t, path)
		if response.StatusCode != http.StatusForbidden || !strings.Contains(body, lockedTitle) {
			t.Errorf("GET %s on a locked page gave %d", path, response.StatusCode)
		}
	}
	_, notice := s.get(t, "/")
	for _, piece := range []string{"It has no password yet.", "SCANBOX_PASSWORD_FILE: /run/secrets/scanbox_password", setup.AllowNoPasswordVariable} {
		if !strings.Contains(notice, piece) {
			t.Errorf("the notice lacks %q", piece)
		}
	}
	changes := map[string]url.Values{
		"/api/settings":       {"field": {"paperless_url"}, "value": {otherAddress}},
		"/api/action":         {"action": {"one_grey_page"}},
		"/api/send-waiting":   {},
		"/api/paperless/test": {},
		signInPath:            {"password": {""}},
		"/signout":            {},
	}
	for path, values := range changes {
		response, body := s.post(t, path, values)
		if response.StatusCode != http.StatusForbidden || !strings.Contains(body, lockedTitle) {
			t.Errorf("POST %s on a locked page gave %d", path, response.StatusCode)
		}
	}
	if address := s.page.options.Settings.Get().PaperlessURL; address != paperlessAddress {
		t.Fatalf("a locked page changed the Paperless address to %s", address)
	}
	if response, body := s.get(t, "/healthz"); response.StatusCode != http.StatusOK || body != "ok\n" {
		t.Fatalf("health on a locked page: %d %s", response.StatusCode, body)
	}
	if response, _ := s.get(t, "/static/app.css"); response.StatusCode != http.StatusOK {
		t.Fatalf("the notice's style sheet gave %d", response.StatusCode)
	}
	if response, _ := s.getAs(t, foreignHost, "/healthz"); response.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("a locked page answered a foreign name with %d", response.StatusCode)
	}
}

func TestAPageNobodyDecidedAboutStaysLocked(t *testing.T) {
	undecided := map[string]Options{
		"a password, but no access decided":  {Password: testPassword, Checker: nobody{}},
		"sign-in asked for, but no password": {Access: setup.BySignIn, Checker: nobody{}},
	}
	for name, options := range undecided {
		s := newSiteFor(t, options)
		if response, body := s.get(t, "/"); response.StatusCode != http.StatusForbidden || !strings.Contains(body, lockedTitle) {
			t.Errorf("%s: the page gave %d", name, response.StatusCode)
		}
		response, _ := s.post(t, signInPath, url.Values{"password": {options.Password}})
		if response.StatusCode != http.StatusForbidden || len(response.Cookies()) != 0 {
			t.Errorf("%s: signing in gave %d and %d cookies", name, response.StatusCode, len(response.Cookies()))
		}
		// A session with a good signature, such as one kept from before the page was locked.
		expiry := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: expiry + "." + s.page.guard.signature(expiry)})
		if s.page.guard.signedIn(request) {
			t.Errorf("%s: an earlier session was let in", name)
		}
	}
	s := newSiteFor(t, Options{Password: testPassword, Access: setup.Open, Checker: nobody{}})
	if response, _ := s.get(t, "/"); response.Request.URL.Path != signInPath {
		t.Fatal("a page with a password was served without signing in")
	}
}

func TestThePageAnswersOnlyToItsOwnNames(t *testing.T) {
	s := newSiteFor(t, Options{Access: setup.Open, Checker: nobody{}, Hosts: []string{listedHost}})
	own := []string{"127.0.0.1:28761", "192.168.10.20", "[::1]:28761", "[::1]", "localhost:28761", "mybox", "mybox:28761", "Scanbox.local",
		"box.lan:28761", "box.home.arpa.", "printer.internal", "dev.localhost", listedHost, "Scan.Example.org:28761"}
	for _, header := range own {
		if !s.page.answersTo(hostName(header)) {
			t.Errorf("the page should answer to %q", header)
		}
	}
	foreign := []string{"", foreignHost, "evil.example", "evil.example.:28761", "192.168.10.20.evil.example", "mybox.evil.example",
		"localhost.evil.example", "scanbox.local.evil.example", listedHost + ".evil.example", "not" + listedHost}
	for _, header := range foreign {
		if s.page.answersTo(hostName(header)) {
			t.Errorf("the page should not answer to %q", header)
		}
	}

	for _, path := range []string{"/", "/settings", "/healthz", "/static/app.css"} {
		response, body := s.getAs(t, foreignHost, path)
		saysWhatToList := strings.Contains(body, strconv.Quote(hostName(foreignHost))) && strings.Contains(body, setup.HostsVariable)
		if response.StatusCode != http.StatusMisdirectedRequest || !saysWhatToList {
			t.Errorf("GET %s under a foreign name gave %d %s", path, response.StatusCode, body)
		}
	}
	// What a page on another site does once it has pointed a name of its own at this box:
	// the change arrives under that name, and says it comes from that name.
	response, _ := s.postAs(t, foreignHost, "/api/settings", url.Values{"field": {"paperless_url"}, "value": {otherAddress}})
	if response.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("a change under a foreign name gave %d", response.StatusCode)
	}
	if address := s.page.options.Settings.Get().PaperlessURL; address != paperlessAddress {
		t.Fatalf("a foreign name moved the Paperless address to %s", address)
	}
	if response, body := s.getAs(t, listedHost, "/"); response.StatusCode != http.StatusOK || !strings.Contains(body, "Single page scan") {
		t.Fatalf("the page under a listed name gave %d", response.StatusCode)
	}
	if response, body := s.postAs(t, listedHost, "/api/settings", url.Values{"field": {"resolution_dpi"}, "value": {"600"}}); response.StatusCode != http.StatusOK {
		t.Fatalf("a change under a listed name gave %d %s", response.StatusCode, body)
	}
}

// answeringTasks is a Paperless whose task list always says the same thing.
type answeringTasks struct {
	nobody
	delivery station.Delivery
	problem  error
}

func (a answeringTasks) Confirm(context.Context, string) (station.Delivery, error) {
	return a.delivery, a.problem
}

// refusingUploads is a Paperless that refuses every file as it is.
type refusingUploads struct {
	nobody
	tries *atomic.Int32
}

func (r refusingUploads) Upload(context.Context, string, string, string, time.Time) (string, error) {
	r.tries.Add(1)
	return "", fmt.Errorf("Paperless refused the file (400 Bad Request: “File type not supported”); %w", station.ErrRejected)
}

func TestAFailedScanCanBeRetried(t *testing.T) {
	tries := &atomic.Int32{}
	s := newSiteFor(t, Options{Access: setup.Open, Checker: refusingUploads{tries: tries}})
	s.post(t, "/api/settings", url.Values{"field": {"review_before_sending"}, "value": {"false"}})
	s.post(t, "/api/action", url.Values{"action": {"one_grey_page"}})
	document := waitForState(t, s, station.Failed)
	_, page := s.get(t, "/")
	recent := between(t, page, `<ul class="rows">`, `</ul>`)
	if !strings.Contains(recent, `data-retry="`+document.ID+`"`) || !strings.Contains(recent, "File type not supported") {
		t.Fatalf("a failed scan should show its reason and Retry:\n%s", recent)
	}
	if response, body := s.post(t, "/api/documents/"+document.ID+"/retry", url.Values{}); response.StatusCode != http.StatusOK {
		t.Fatalf("retry: %d %s", response.StatusCode, body)
	}
	deadline := time.Now().Add(waitLimit)
	for tries.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("Retry didn't send the scan again")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if response, _ := s.post(t, "/api/send-waiting", url.Values{}); response.StatusCode != http.StatusOK {
		t.Fatal("Send now was refused")
	}
}

// holdingUploads is a Paperless whose uploads wait until released.
type holdingUploads struct {
	nobody
	started, release chan struct{}
}

func (h holdingUploads) Upload(context.Context, string, string, string, time.Time) (string, error) {
	close(h.started)
	<-h.release
	return "task", nil
}

func TestAScanBeingSentSaysSoAndCantBeDeleted(t *testing.T) {
	paperless := holdingUploads{started: make(chan struct{}), release: make(chan struct{})}
	s := newSiteFor(t, Options{Access: setup.Open, Checker: paperless})
	s.post(t, "/api/settings", url.Values{"field": {"review_before_sending"}, "value": {"false"}})
	s.post(t, "/api/action", url.Values{"action": {"one_grey_page"}})
	<-paperless.started
	defer close(paperless.release)
	_, page := s.get(t, "/")
	recent := between(t, page, `<ul class="rows">`, `</ul>`)
	if !strings.Contains(recent, `<span class="state warn">Sending…</span>`) || !strings.Contains(recent, "on its way to Paperless") {
		t.Fatalf("a scan being sent:\n%s", recent)
	}
	document := s.station.Status().Waiting[0]
	response, body := s.post(t, "/api/documents/"+document.ID+"/delete", url.Values{})
	if response.StatusCode != http.StatusConflict || !strings.Contains(body, "being sent to Paperless right now") {
		t.Fatalf("deleting a scan being sent: %d %s", response.StatusCode, body)
	}
}

func waitForState(t *testing.T, s *site, state station.State) station.Document {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		if found := s.station.Search(station.Query{State: state, Clock: s.page.options.Settings.Get().Clock()}); len(found) == 1 {
			return found[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no scan reached %s", state)
	return station.Document{}
}

func TestThePageSaysWhetherPaperlessStoredAScan(t *testing.T) {
	const reason = "Paperless refused it as a copy of its document 321"
	failing := newSiteFor(t, Options{Access: setup.Open, Checker: answeringTasks{delivery: station.Delivery{Failure: reason, Listed: true}}})
	failing.post(t, "/api/settings", url.Values{"field": {"review_before_sending"}, "value": {"false"}})
	failing.post(t, "/api/action", url.Values{"action": {"one_grey_page"}})
	waitForState(t, failing, station.Failed)
	_, page := failing.get(t, "/")
	recent := between(t, page, `<ul class="rows">`, `</ul>`)
	if !strings.Contains(recent, `<span class="state error">Failed</span>`) || !strings.Contains(recent, reason) || strings.Contains(recent, "Open in Paperless") {
		t.Fatalf("a failed scan on the Scanner page:\n%s", recent)
	}

	pending := newSiteFor(t, Options{Access: setup.Open, Checker: answeringTasks{delivery: station.Delivery{Listed: true}}})
	pending.post(t, "/api/settings", url.Values{"field": {"review_before_sending"}, "value": {"false"}})
	pending.post(t, "/api/action", url.Values{"action": {"one_grey_page"}})
	waitForState(t, pending, station.Submitted)
	_, page = pending.get(t, "/scans?state=submitted")
	if !strings.Contains(page, `<span class="state warn">Accepted</span>`) || !strings.Contains(page, "Paperless is storing it") || strings.Contains(page, "Open in Paperless") {
		t.Fatal("an accepted scan should say Paperless is storing it, without a link yet")
	}

	denied := newSiteFor(t, Options{Access: setup.Open, Checker: answeringTasks{problem: fmt.Errorf("asking: %w", paperless.ErrNoTaskAccess)}})
	denied.post(t, "/api/settings", url.Values{"field": {"review_before_sending"}, "value": {"false"}})
	denied.post(t, "/api/action", url.Values{"action": {"one_grey_page"}})
	waitForState(t, denied, station.Submitted)
	deadline := time.Now().Add(waitLimit)
	for !errors.Is(denied.station.Status().ConfirmError, paperless.ErrNoTaskAccess) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_, page = denied.get(t, "/")
	if !strings.Contains(page, "Paperless can&#39;t confirm 1 scan") || !strings.Contains(page, "tick View for PaperlessTask") {
		t.Fatal("the status line should say which permission is missing")
	}
	_, settingsPage := denied.get(t, "/settings")
	if !strings.Contains(settingsPage, "Can&#39;t view tasks") {
		t.Fatal("the setup checker should say the token's user can't view tasks")
	}
}

func TestScanReviewNameAndSend(t *testing.T) {
	s := newSite(t, "")
	if response, body := s.post(t, "/api/action", url.Values{"action": {"one_grey_page"}}); response.StatusCode != http.StatusOK {
		t.Fatalf("scan: %d %s", response.StatusCode, body)
	}
	document := waitForReview(t, s)
	if response, _ := s.get(t, "/documents/"+document.ID+".pdf"); response.Header.Get("Content-Type") != "application/pdf" {
		t.Fatal("the PDF isn't served")
	}
	if response, _ := s.get(t, "/documents/"+document.ID+"/pages/1.jpg"); response.StatusCode != http.StatusOK {
		t.Fatal("the first page picture isn't served")
	}
	response, preview := s.get(t, "/documents/"+document.ID+"/previews/1.jpg")
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "image/jpeg" || len(preview) < 1000 {
		t.Fatalf("the page preview: %d %s, %d bytes", response.StatusCode, response.Header.Get("Content-Type"), len(preview))
	}
	if response, _ := s.get(t, "/documents/"+document.ID+"/previews/2.jpg"); response.StatusCode != http.StatusNotFound {
		t.Fatal("a page past the end was served")
	}
	_, page := s.get(t, "/")
	if !strings.Contains(page, `data-view="`+document.ID+`"`) || !strings.Contains(page, `<dialog class="viewer"`) {
		t.Fatal("the Scanner page has no viewer for the scan")
	}
	_, body := s.post(t, "/api/documents/"+document.ID+"/name", url.Values{"name": {"a/b"}})
	var answer struct {
		OK      bool
		Message string
	}
	json.Unmarshal([]byte(body), &answer)
	if answer.OK || strings.Contains(answer.Message, "invalid setting") {
		t.Fatalf("bad name answer: %s", body)
	}
	s.post(t, "/api/documents/"+document.ID+"/name", url.Values{"name": {"Gas bill November"}})
	s.post(t, "/api/documents/"+document.ID+"/send", url.Values{})
	deadline := time.Now().Add(waitLimit)
	for len(s.station.Status().Sent) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("never sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if name := s.station.Status().Sent[0].Name; name != "Gas bill November" {
		t.Fatalf("sent as %q", name)
	}
	_, body = s.get(t, "/")
	if !strings.Contains(body, "Gas bill November") || !strings.Contains(body, "In Paperless") {
		t.Fatal("the Scanner page doesn't list the sent scan")
	}
}

func TestSettingsSaveOneFieldAtATime(t *testing.T) {
	s := newSite(t, "")
	if response, _ := s.post(t, "/api/settings", url.Values{"field": {"resolution_dpi"}, "value": {"600"}}); response.StatusCode != http.StatusOK {
		t.Fatal("a valid setting was refused")
	}
	response, body := s.post(t, "/api/settings", url.Values{"field": {"idle_finish_minutes"}, "value": {"0"}})
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(body, "whole number from 1 to 240") {
		t.Fatalf("an invalid setting: %d %s", response.StatusCode, body)
	}
	_, page := s.get(t, "/settings")
	if !strings.Contains(page, `value="600" data-field="resolution_dpi" checked`) {
		t.Fatal("the Settings page doesn't show the saved resolution")
	}
	if !strings.Contains(page, "Container setup") || !strings.Contains(page, "1 item to add") || !strings.Contains(page, "SCANBOX_PASSWORD_FILE") {
		t.Fatal("without a password the setup checker should ask for one, with lines to copy")
	}
}

func TestLiveUpdatesStream(t *testing.T) {
	s := newSite(t, "")
	request, _ := http.NewRequest(http.MethodGet, s.server.URL+"/live", nil)
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	response, err := s.client.Do(request.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	buffer := make([]byte, 64*1024)
	read, _ := response.Body.Read(buffer)
	line := strings.TrimPrefix(strings.SplitN(string(buffer[:read]), "\n", 2)[0], "data: ")
	var parts map[string]string
	if err := json.Unmarshal([]byte(line), &parts); err != nil {
		t.Fatalf("not JSON: %q", line)
	}
	if !strings.Contains(parts["badge"], "status-badge") || !strings.Contains(parts["badge"], "Test scanner") {
		t.Fatalf("no status in the header part: %v", parts["badge"])
	}
	if !strings.Contains(parts["footer"], "Test scanner · fake driver · no scanning library") {
		t.Fatalf("the foot doesn't say what the scanner reports about itself: %v", parts["footer"])
	}
}

func TestStaticFilesAreServedButNotListed(t *testing.T) {
	s := newSite(t, "")
	if response, _ := s.get(t, "/static/"); response.StatusCode != http.StatusNotFound {
		t.Fatalf("/static/ answered %d; it should list nothing", response.StatusCode)
	}
	if response, body := s.get(t, "/static/app.css"); response.StatusCode != http.StatusOK || !strings.Contains(body, "--warn") {
		t.Fatalf("/static/app.css answered %d", response.StatusCode)
	}
}

func TestEveryChangeIsCappedInSize(t *testing.T) {
	s := newSite(t, "")
	huge := url.Values{"name": {strings.Repeat("a", formLimitBytes)}}
	for _, path := range []string{"/api/documents/any/name", "/api/action", "/api/settings"} {
		if response, _ := s.post(t, path, huge); response.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("POST %s with more than %d bytes answered %d", path, formLimitBytes, response.StatusCode)
		}
	}
}

func TestTheDocumentInProgressCanBeReordered(t *testing.T) {
	s := newSite(t, "")
	for count := 1; count <= 2; count++ {
		s.post(t, "/api/action", url.Values{"action": {"add_grey_page"}})
		deadline := time.Now().Add(waitLimit)
		for s.station.Status().OpenPages < count && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
	}
	numbers := s.station.Status().OpenPageNumbers
	if len(numbers) != 2 {
		t.Fatalf("open pages %v, want 2", numbers)
	}
	first, second := strconv.Itoa(numbers[0]), strconv.Itoa(numbers[1])
	_, page := s.get(t, "/")
	strip := between(t, page, `<ol class="page-strip">`, `</ol>`)
	for _, button := range []string{
		`data-move="` + first + `" data-direction="earlier" aria-label="Move page 1 earlier" title="Move earlier" disabled`,
		`data-move="` + second + `" data-direction="later" aria-label="Move page 2 later" title="Move later" disabled`,
		`data-move="` + second + `" data-direction="earlier" aria-label="Move page 2 earlier" title="Move earlier" >`,
	} {
		if !strings.Contains(strip, button) {
			t.Fatalf("the page strip lacks %q:\n%s", button, strip)
		}
	}
	versionBefore := between(t, strip, `.jpg?v=`, `"`)
	if response, body := s.post(t, "/api/open/pages/"+second+"/move", url.Values{"direction": {"earlier"}}); response.StatusCode != http.StatusOK {
		t.Fatalf("moving page 2 earlier: %d %s", response.StatusCode, body)
	}
	if order := s.station.Status().OpenPageNumbers; !slices.Equal(order, []int{numbers[1], numbers[0]}) {
		t.Fatalf("after the move the order is %v", order)
	}
	_, page = s.get(t, "/")
	if versionAfter := between(t, page, `.jpg?v=`, `"`); versionAfter == versionBefore {
		t.Fatal("the pictures' version didn't change, so the browser would keep showing them in the old order")
	}
	for path, direction := range map[string]string{"/api/open/pages/" + second + "/move": "earlier", "/api/open/pages/" + first + "/move": "sideways"} {
		if response, _ := s.post(t, path, url.Values{"direction": {direction}}); response.StatusCode != http.StatusBadRequest {
			t.Errorf("POST %s %s answered %d, want 400", path, direction, response.StatusCode)
		}
	}
}

// nextEvent reads the live stream up to its next update.
func nextEvent(t *testing.T, stream *bufio.Reader) map[string]string {
	t.Helper()
	for {
		line, err := stream.ReadString('\n')
		if err != nil {
			t.Fatalf("the stream ended: %v", err)
		}
		data, isData := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !isData {
			continue
		}
		var parts map[string]string
		if err := json.Unmarshal([]byte(data), &parts); err != nil {
			t.Fatalf("not JSON: %q", data)
		}
		return parts
	}
}

// After the first update, which carries every part, an update carries only the parts
// that changed, with the review key the page compares.
func TestLiveUpdatesCarryOnlyWhatChanged(t *testing.T) {
	s := newSite(t, "")
	request, _ := http.NewRequest(http.MethodGet, s.server.URL+"/live", nil)
	ctx, cancel := context.WithTimeout(context.Background(), waitLimit)
	defer cancel()
	response, err := s.client.Do(request.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	stream := bufio.NewReader(response.Body)
	first := nextEvent(t, stream)
	for _, name := range append(liveParts, reviewKeyPart) {
		if _, carried := first[name]; !carried {
			t.Fatalf("the first update lacks %q, which an empty part would have too", name)
		}
	}
	s.post(t, "/api/settings", url.Values{"field": {"colour"}, "value": {"false"}})
	changed := nextEvent(t, stream)
	names := slices.Sorted(maps.Keys(changed))
	if !slices.Equal(names, []string{"controls", "glass", reviewKeyPart}) {
		t.Fatalf("switching to greyscale should redraw the controls and the glass only, the update carried %v", names)
	}
}

// versionsOnly is a Paperless that reports versions no one would type in by hand.
type versionsOnly struct{ nobody }

func (versionsOnly) Probe(context.Context) (paperless.ServerInfo, error) {
	return paperless.ServerInfo{Version: "9.8.7-reported", APIVersion: "42"}, nil
}

func TestThePageShowsOnlyWhatIsReported(t *testing.T) {
	s := newSiteWith(t, "", versionsOnly{})
	_, page := s.get(t, "/")
	if !strings.Contains(page, "Paperless not asked yet") || strings.Contains(page, "9.8.7") {
		t.Fatal("before Paperless was asked, the page claimed a version")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.page.WatchPaperless(ctx)
	deadline := time.Now().Add(waitLimit)
	for !strings.Contains(page, "Paperless 9.8.7-reported · API 42 · answered ") {
		if time.Now().After(deadline) {
			t.Fatal("the foot never showed the versions Paperless reported")
		}
		time.Sleep(10 * time.Millisecond)
		_, page = s.get(t, "/")
	}
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(program)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	if !strings.Contains(page, "build "+hex.EncodeToString(sum[:])[:buildIDLength]) {
		t.Fatal("the build shown is not the running program's")
	}
	for _, reported := range []string{"Test scanner · fake driver · no scanning library", "pretend flatbed scanner"} {
		if !strings.Contains(page, reported) {
			t.Errorf("the page lacks what the scanner reports: %q", reported)
		}
	}
}

func TestEveryScanCarriesViewDownloadEditDelete(t *testing.T) {
	s := newSite(t, "")
	s.post(t, "/api/action", url.Values{"action": {"one_grey_page"}})
	document := waitForReview(t, s)
	actions := []string{`data-view="` + document.ID + `"`, `href="/documents/` + document.ID + `/download"`, `data-edit="` + document.ID + `"`, `data-delete="` + document.ID + `"`}
	hasAll := func(where, page string) {
		t.Helper()
		for _, action := range actions {
			if !strings.Contains(page, action) {
				t.Errorf("%s lacks %s", where, action)
			}
		}
	}
	_, page := s.get(t, "/")
	hasAll("a scan to review", between(t, page, `<ul class="cards">`, `</ul>`))
	s.post(t, "/api/documents/"+document.ID+"/send", url.Values{})
	deadline := time.Now().Add(waitLimit)
	for len(s.station.Status().Sent) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("never sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, page = s.get(t, "/")
	hasAll("a recent scan", between(t, page, `<ul class="rows">`, `</ul>`))
	_, page = s.get(t, "/scans")
	hasAll("a scan on the Scans page", between(t, page, `<ul class="rows">`, `</ul>`))
}

// between gives the part of a page from one marker to the next marker after it.
func between(t *testing.T, page, start, end string) string {
	t.Helper()
	_, after, found := strings.Cut(page, start)
	if !found {
		t.Fatalf("the page has no %s", start)
	}
	part, _, _ := strings.Cut(after, end)
	return part
}

func TestASentScanCanBeRenamedWhilePaperlessKeepsItsTitle(t *testing.T) {
	s := newSite(t, "")
	s.post(t, "/api/action", url.Values{"action": {"one_grey_page"}})
	document := waitForReview(t, s)
	s.post(t, "/api/documents/"+document.ID+"/name", url.Values{"name": {"Gas bill"}})
	s.post(t, "/api/documents/"+document.ID+"/send", url.Values{})
	deadline := time.Now().Add(waitLimit)
	for len(s.station.Status().Sent) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("never sent")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if response, body := s.post(t, "/api/documents/"+document.ID+"/name", url.Values{"name": {"Gas bill, paid"}}); response.StatusCode != http.StatusOK {
		t.Fatalf("renaming a sent scan: %d %s", response.StatusCode, body)
	}
	_, page := s.get(t, "/scans")
	if !strings.Contains(page, "Gas bill, paid") || !strings.Contains(page, fmt.Sprintf(`href="%s/documents/%d/details"`, paperlessAddress, storedDocumentID)) {
		t.Fatal("the Scans page should show the new name, and open the document Paperless made")
	}
	if !strings.Contains(page, "In Paperless it stays “Gas bill”.") {
		t.Fatal("Edit doesn't say that Paperless keeps its title")
	}
	if response, _ := s.post(t, "/api/documents/"+document.ID+"/name", url.Values{"name": {"a/b"}}); response.StatusCode != http.StatusBadRequest {
		t.Fatal("a name with a slash was accepted")
	}
}

func TestTheArchiveGroupsScansByDay(t *testing.T) {
	s := newSite(t, "")
	s.post(t, "/api/action", url.Values{"action": {"one_grey_page"}})
	document := waitForReview(t, s)
	s.post(t, "/api/documents/"+document.ID+"/name", url.Values{"name": {"Gas bill November"}})
	writer := clock.New(clock.DefaultTimeZone, "", "")
	day := document.Created.In(writer.Location)
	_, page := s.get(t, "/")
	archive := between(t, page, `<div class="tree">`, `<p class="archive-foot">`)
	for _, piece := range []string{day.Format("2006"), day.Month().String(), writer.ShortDay(day), "today", "Gas bill November", `data-view="` + document.ID + `"`, "/scans?day=" + day.Format("02-01-2006")} {
		if !strings.Contains(archive, piece) {
			t.Errorf("the archive lacks %q", piece)
		}
	}
	_, page = s.get(t, "/scans?day="+day.Format("02-01-2006"))
	if !strings.Contains(page, "1 scan found") || !strings.Contains(page, `aria-current="true"`) {
		t.Fatal("the day from the archive doesn't show its scans")
	}
	_, page = s.get(t, "/scans?day="+day.AddDate(0, 0, -1).Format("02-01-2006"))
	if !strings.Contains(page, "0 scans found") {
		t.Fatal("the day before shows scans it doesn't hold")
	}
}

func TestHealthNeedsNoSignInAndSaysNoMore(t *testing.T) {
	s := newSite(t, testPassword)
	response, body := s.get(t, "/healthz")
	if response.StatusCode != http.StatusOK || body != "ok\n" {
		t.Fatalf("health: %d %q", response.StatusCode, body)
	}
	if response, _ := s.get(t, "/api/status"); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the detail should need a sign-in: %d", response.StatusCode)
	}
	s.post(t, signInPath, url.Values{"password": {testPassword}})
	response, body = s.get(t, "/api/status")
	var detail map[string]any
	if err := json.Unmarshal([]byte(body), &detail); err != nil || detail["health"] != "ok" || detail["scanner"] != "Test scanner" {
		t.Fatalf("the detail for someone signed in: %d %s", response.StatusCode, body)
	}
}

func TestSignInsAndChangesAreLogged(t *testing.T) {
	var written bytes.Buffer
	log.SetOutput(&written)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	s := newSite(t, testPassword)
	s.post(t, signInPath, url.Values{"password": {"wrong guess"}})
	s.post(t, signInPath, url.Values{"password": {testPassword}})
	s.post(t, "/api/settings", url.Values{"field": {"resolution_dpi"}, "value": {"600"}})
	s.post(t, "/signout", url.Values{})
	logged := written.String()
	for _, line := range []string{"sign-in: wrong password from 127.0.0.1", "sign-in from 127.0.0.1", "settings: resolution_dpi changed", "sign-out from 127.0.0.1: every session ended"} {
		if !strings.Contains(logged, line) {
			t.Errorf("the log lacks %q:\n%s", line, logged)
		}
	}
	if strings.Contains(logged, testPassword) || strings.Contains(logged, "wrong guess") {
		t.Fatal("a password reached the log")
	}
}

func waitForReview(t *testing.T, s *site) station.Document {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		if review := s.station.Status().Review; len(review) == 1 {
			return review[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no scan to review")
	return station.Document{}
}

func TestDownloadAnyTimeUnderTheScansName(t *testing.T) {
	s := newSite(t, "")
	s.post(t, "/api/action", url.Values{"action": {"one_grey_page"}})
	document := waitForReview(t, s)
	s.post(t, "/api/documents/"+document.ID+"/name", url.Values{"name": {"Gas bill November"}})
	response, body := s.get(t, "/documents/"+document.ID+"/download")
	disposition := response.Header.Get("Content-Disposition")
	if response.StatusCode != http.StatusOK || !strings.HasPrefix(body, "%PDF") || !strings.Contains(disposition, "attachment") || !strings.Contains(disposition, "Gas bill November.pdf") {
		t.Fatalf("download before sending: %d %q", response.StatusCode, disposition)
	}
	s.post(t, "/api/documents/"+document.ID+"/send", url.Values{})
	deadline := time.Now().Add(waitLimit)
	for len(s.station.Status().Sent) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if response, _ := s.get(t, "/documents/"+document.ID+"/download"); response.StatusCode != http.StatusOK {
		t.Fatal("a sent scan can't be downloaded")
	}
	if response, _ := s.get(t, "/documents/../settings.json/download"); response.StatusCode == http.StatusOK {
		t.Fatal("a path outside the scans was served")
	}
}

func TestScansPageSearchesWithEuropeanDates(t *testing.T) {
	s := newSite(t, "")
	s.post(t, "/api/action", url.Values{"action": {"one_grey_page"}})
	document := waitForReview(t, s)
	s.post(t, "/api/documents/"+document.ID+"/name", url.Values{"name": {"Gas bill November"}})
	created := document.Created.Local()

	_, page := s.get(t, "/scans?q=gas")
	if !strings.Contains(page, "Gas bill November") || !strings.Contains(page, created.Format("Monday 2 January 2006")) {
		t.Fatal("search by name didn't find the scan under a European day heading")
	}
	_, page = s.get(t, "/scans?q="+url.QueryEscape(created.Format("02/01/2006")))
	if !strings.Contains(page, "Gas bill November") {
		t.Fatal("search by a day/month/year date found nothing")
	}
	_, page = s.get(t, "/scans?q=water")
	if !strings.Contains(page, "Nothing matches") {
		t.Fatal("a search without results says nothing")
	}
	_, page = s.get(t, "/scans?month="+created.Format("01-2006"))
	if !strings.Contains(page, "Gas bill November") || !strings.Contains(page, `aria-current="true"`) {
		t.Fatal("the month folder doesn't show its scans")
	}
	_, page = s.get(t, "/scans?from=2026-13-40")
	if !strings.Contains(page, "Write dates day first") {
		t.Fatal("a wrong date gave no advice")
	}
	_, page = s.get(t, "/settings")
	if !strings.Contains(page, "Scans use") || !strings.Contains(page, `<option value="0" selected>Always</option>`) {
		t.Fatal("Settings doesn't show the space used or the keep-always default")
	}
}

func TestAnAddressThatWouldExposeTheTokenIsRefused(t *testing.T) {
	s := newSite(t, "")
	response, body := s.post(t, "/api/settings", url.Values{"field": {"paperless_url"}, "value": {"http://paperless.example.com"}})
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(body, "use https") {
		t.Fatalf("an http address on the internet: %d %s", response.StatusCode, body)
	}
	if address := s.page.options.Settings.Get().PaperlessURL; address != paperlessAddress {
		t.Fatalf("the refused address replaced the saved one: %q", address)
	}
}

func TestDatingByScanDayIsASetting(t *testing.T) {
	s := newSite(t, "")
	_, page := s.get(t, "/settings")
	if !strings.Contains(page, `<input type="checkbox" data-field="send_scan_date" >`) {
		t.Fatal("Settings should offer dating by scan day, off at first")
	}
	s.post(t, "/api/settings", url.Values{"field": {"send_scan_date"}, "value": {"true"}})
	_, page = s.get(t, "/settings")
	if !strings.Contains(page, `data-field="send_scan_date" checked`) {
		t.Fatal("the Settings page doesn't show dating by scan day as on")
	}
}

func TestAStatusTitleEndsAsASentenceOnce(t *testing.T) {
	for title, want := range map[string]string{"Scanning…": "Scanning…", "Space is running out": "Space is running out."} {
		if got := headline(title); got != want {
			t.Errorf("headline(%q) = %q, want %q", title, got, want)
		}
	}
}

func TestAPageBeingSavedIsShown(t *testing.T) {
	view := describeStatus(station.Status{Scanner: "Test scanner", Saving: true}, settings.Defaults())
	if view.Title != "Saving the page…" || !view.Busy {
		t.Fatalf("a page being saved: %+v", view)
	}
}

func TestAPressWaitingForTheScanIsShown(t *testing.T) {
	view := describeStatus(station.Status{Scanner: "Test scanner", Scanning: true, Queued: true}, settings.Defaults())
	if view.Title != "Scanning…" || view.Detail != "The next page you asked for scans as soon as this one is done." {
		t.Fatalf("a press waiting for the scan under way: %+v", view)
	}
}

func TestTheReserveIsASettingAndLowSpaceIsShown(t *testing.T) {
	s := newSite(t, "")
	_, page := s.get(t, "/settings")
	if !strings.Contains(page, `data-field="reserve_mb"`) || !strings.Contains(page, `<option value="200" selected>200 MB</option>`) {
		t.Fatal("Settings should offer the space to keep free, 200 MB at first")
	}
	if response, _ := s.post(t, "/api/settings", url.Values{"field": {"reserve_mb"}, "value": {"1000"}}); response.StatusCode != http.StatusOK || s.page.options.Settings.Get().ReserveMB != 1000 {
		t.Fatal("a reserve of 1 GB was refused")
	}
	view := describeStatus(station.Status{Scanner: "Test scanner", LowSpace: "only 150 MB is free on the box, less than the 200 MB kept free, so scanning has stopped. Delete old scans in Scans, or free space on the box"}, s.page.options.Settings.Get())
	if view.Title != "Space is running out" || view.Dot != "warn" || !strings.HasPrefix(view.Detail, "Only 150 MB is free") || !strings.HasSuffix(view.Detail, "box.") {
		t.Fatalf("the status line: %+v", view)
	}
}

func TestScanFactsFollowTheChosenStyles(t *testing.T) {
	london := clock.New("Europe/London", "", "")
	created := time.Date(2026, 4, 3, 8, 5, 0, 0, time.UTC)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	if facts := documentFacts(station.Document{Pages: 2, Colour: "colour", Created: created}, now, london); facts != "2 pages · colour · 03/04/2026 09:05" {
		t.Fatalf("default facts %q", facts)
	}
	desktop := clock.New("Europe/London", "mon-d-y", "12")
	if facts := documentFacts(station.Document{Pages: 1, Colour: "greyscale", Created: created}, now, desktop); facts != "1 page · greyscale · Apr 3 2026 9:05 am" {
		t.Fatalf("desktop-style facts %q", facts)
	}
	if facts := documentFacts(station.Document{Pages: 1, Colour: "colour", Created: now}, now, london); facts != "1 page · colour · 12:00" {
		t.Fatalf("today's facts should show only the time: %q", facts)
	}
}

func TestDateAndTimeSettings(t *testing.T) {
	s := newSite(t, "")
	_, page := s.get(t, "/settings")
	for _, piece := range []string{"Date and time", `<option value="UTC" selected>`, `<option value="dmy-slash" selected>`, "(day/month/year)", `<option value="24" selected>`, "(24-hour)"} {
		if !strings.Contains(page, piece) {
			t.Errorf("Settings lacks %q", piece)
		}
	}
	s.post(t, "/api/settings", url.Values{"field": {"date_style"}, "value": {"mon-d-y"}})
	if response, body := s.post(t, "/api/settings", url.Values{"field": {"time_zone"}, "value": {"Mars/Olympus"}}); response.StatusCode != http.StatusBadRequest || !strings.Contains(body, "time zone") {
		t.Fatalf("an unknown zone: %d %s", response.StatusCode, body)
	}
	_, preview := s.get(t, "/api/date-time")
	var view dateTimeView
	if err := json.Unmarshal([]byte(preview), &view); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(view.Name, "Scan ") || strings.ContainsAny(view.Name, "/:") || !strings.Contains(view.Recent, " 20") {
		t.Fatalf("preview after choosing month-name dates: %+v", view)
	}
}
