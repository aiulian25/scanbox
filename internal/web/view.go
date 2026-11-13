package web

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"scanbox/internal/clock"
	"scanbox/internal/paperless"
	"scanbox/internal/scanner"
	"scanbox/internal/settings"
	"scanbox/internal/station"
)

const (
	// monthParamLayout names a month folder in links: 11-2026, month then year, never a day.
	monthParamLayout = "01-2006"
	unreachableText  = "Paperless can't be reached"
	ellipsis         = "…"
	kilobyte         = 1024
)

var keepLabels = map[int]string{0: "Always", 7: "7 days", 30: "30 days", 90: "90 days", 365: "1 year"}

var reserveLabels = map[int]string{100: "100 MB", 200: "200 MB", 500: "500 MB", 1000: "1 GB", 2000: "2 GB"}

// readableSize writes a byte count as KB, MB or GB.
func readableSize(bytes int64) string {
	units := []string{"bytes", "KB", "MB", "GB", "TB"}
	size, unit := float64(bytes), 0
	for size >= kilobyte && unit < len(units)-1 {
		size, unit = size/kilobyte, unit+1
	}
	if unit == 0 {
		return fmt.Sprintf("%d %s", bytes, units[0])
	}
	return fmt.Sprintf("%.1f %s", size, units[unit])
}

var actionLabels = map[settings.Action]string{
	settings.AddColourPage:  "Multi page scan, colour",
	settings.AddGreyPage:    "Multi page scan, greyscale",
	settings.FinishDocument: "Finish document",
	settings.OneColourPage:  "Single page scan, colour",
	settings.OneGreyPage:    "Single page scan, greyscale",
	settings.DoNothing:      "Nothing",
}

var resolutionNotes = map[int]string{
	150: "smaller files",
	300: "recommended for documents",
	600: "small print and photos",
}

var paperLabels = map[settings.PaperSize]string{
	settings.PaperA4:     "A4",
	settings.PaperLetter: "US Letter",
	settings.PaperGlass:  "Whole glass",
}

type statusView struct {
	Dot      string
	Title    string
	Detail   string
	LinkText string
	LinkHref string
	Problem  string
	Busy     bool
}

// describeStatus turns the station's state into one headline and one sentence.
func describeStatus(status station.Status, current settings.Settings) statusView {
	view := statusView{Problem: firstClause(status.ScanError)}
	switch {
	case status.Scanner == "":
		view.Dot, view.Title = "error", "Scanner not found"
		view.Detail = "Check that its USB cable is plugged in and it's switched on. Scan Box finds it again by itself."
		view.Problem = ""
	case status.Scanning && status.Queued:
		view.Title, view.Detail, view.Busy = "Scanning…", "The next page you asked for scans as soon as this one is done.", true
	case status.Scanning:
		view.Title, view.Detail, view.Busy = "Scanning…", "Keep the lid closed until the scanner stops.", true
	case status.Saving:
		view.Title, view.Detail, view.Busy = "Saving the page…", "The scanner is done; the page appears in a moment.", true
	case status.LowSpace != "":
		view.Dot, view.Title, view.Detail = "warn", "Space is running out", sentence(status.LowSpace)
	case current.PaperlessURL == "":
		view.Dot, view.Title = "warn", "One step left"
		view.Detail = "Add your Paperless address to start sending scans."
		view.LinkText, view.LinkHref = "Add it now", "/settings#paperless"
	case status.UploadError != "" && len(status.Waiting) > 0:
		view.Dot, view.Title = "warn", uploadTitle(status.UploadError)
		view.Detail = fmt.Sprintf("%s waiting and will be sent automatically. Scanning still works.", countWords(len(status.Waiting), "scan is", "scans are"))
	case status.ConfirmError != nil && len(status.Submitted) > 0:
		view.Dot, view.Title = "warn", "Paperless can't confirm "+countWords(len(status.Submitted), "scan", "scans")
		view.Detail = confirmDetail(status.ConfirmError)
	default:
		view.Title, view.Detail = "Ready", status.Scanner
	}
	return view
}

// confirmDetail says why accepted scans can't be confirmed. A missing permission is
// explained in full, with what to grant; anything else by its first clause.
func confirmDetail(problem error) string {
	if errors.Is(problem, paperless.ErrNoTaskAccess) {
		return sentence(problem.Error())
	}
	return sentence(firstClause(problem.Error())) + " Scan Box asks again every minute."
}

// headline ends a status title as a sentence before its detail, unless it trails off
// already, as "Scanning…" does.
func headline(title string) string {
	if strings.HasSuffix(title, ellipsis) {
		return title
	}
	return title + "."
}

// sentence starts a message with a capital letter and ends it with a full stop.
func sentence(message string) string {
	if message == "" {
		return message
	}
	first, size := utf8.DecodeRuneInString(message)
	return string(unicode.ToUpper(first)) + strings.TrimSuffix(message[size:], ".") + "."
}

func uploadTitle(message string) string {
	if strings.HasPrefix(message, unreachableText) {
		return unreachableText
	}
	return "Sending to Paperless failed: " + firstClause(message)
}

// firstClause drops the technical tail that Go errors carry after the first colon.
func firstClause(message string) string {
	clause, _, _ := strings.Cut(message, ": ")
	return clause
}

func countWords(count int, one, many string) string {
	if count == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", count, many)
}

type documentView struct {
	ID             string
	Name           string
	Pages          int
	Facts          string
	Time           string
	Size           string
	State          string
	StateClass     string
	Problem        string
	Link           string
	Waiting        bool
	Reviewing      bool
	Sent           bool
	Failed         bool
	CanSend        bool
	DeleteQuestion string
	EditNote       string
}

// describeDocument writes a scan's facts for the page. canSend says whether Paperless's
// address and token are set, so a scan waiting for review can be sent.
func describeDocument(document station.Document, paperlessURL string, canSend bool, now time.Time, writer clock.Clock) documentView {
	view := documentView{
		ID: document.ID, Name: document.Name, Pages: document.Pages, Facts: documentFacts(document, now, writer),
		Time: writer.Time(document.Created), Size: readableSize(document.Size), CanSend: canSend,
		DeleteQuestion: "Delete this scan? It hasn't been sent to Paperless, so it will be gone for good.",
		EditNote:       "Paperless gets this name as the title when the scan is sent.",
	}
	if document.Sending {
		view.State, view.StateClass = "Sending…", "warn"
		view.Problem = "on its way to Paperless"
		view.DeleteQuestion = "This scan is being sent to Paperless right now. It can be deleted once that is done."
		view.EditNote = "This renames Scan Box's copy. Paperless gets “" + document.Name + "”, which is on its way already."
		return view
	}
	switch document.State {
	case station.ToReview:
		view.State, view.StateClass, view.Reviewing = "To review", "review", true
	case station.Waiting:
		view.State, view.StateClass, view.Waiting = "Waiting", "warn", true
		view.Problem = withNextTry(firstClause(document.LastError), document.NextTry, now, writer)
		view.DeleteQuestion = "Delete this scan? It may already be on its way to Paperless; Scan Box's copy will be gone."
	case station.Submitted:
		view.State, view.StateClass = "Accepted", "warn"
		view.Problem = "Paperless is storing it"
		view.DeleteQuestion = "Delete Scan Box's copy? Paperless has accepted the scan and is storing it."
		view.EditNote = keptTitleNote(document)
	case station.Failed:
		view.State, view.StateClass, view.Failed = "Failed", "error", true
		view.Problem = document.LastError
		view.DeleteQuestion = "Delete this scan? Paperless didn't store it, so it will be gone for good."
	case station.Sent:
		view.State, view.StateClass, view.Sent = "In Paperless", "ok", true
		view.DeleteQuestion = "Delete Scan Box's copy? The document stays in Paperless."
		view.EditNote = keptTitleNote(document)
		view.Link = paperlessLink(document, paperlessURL)
	}
	return view
}

// withNextTry adds when a scan waiting out a pause is sent again.
func withNextTry(problem string, nextTry, now time.Time, writer clock.Clock) string {
	if !nextTry.After(now) {
		return problem
	}
	again := "tries again at " + writer.Time(nextTry)
	if problem == "" {
		return again
	}
	return problem + " · " + again
}

func keptTitleNote(document station.Document) string {
	return "This renames Scan Box's copy only. In Paperless it stays “" + document.PaperlessTitle() + "”."
}

// paperlessLink opens the document Paperless made. A scan sent by an older version, before
// Paperless was asked for the document's number, is found by its title instead.
func paperlessLink(document station.Document, paperlessURL string) string {
	if paperlessURL == "" {
		return ""
	}
	if document.DocumentID != 0 {
		return fmt.Sprintf("%s/documents/%d/details", paperlessURL, document.DocumentID)
	}
	return paperlessURL + "/documents?title_content=" + urlQuery(document.PaperlessTitle())
}

// documentFacts gives a scan's pages, colour and time, with the date if it isn't today.
func documentFacts(document station.Document, now time.Time, writer clock.Clock) string {
	pages := countWords(document.Pages, "page", "pages")
	when := writer.Time(document.Created)
	if !writer.SameDay(document.Created, now) {
		when = writer.DateTime(document.Created)
	}
	return pages + " · " + document.Colour + " · " + when
}

type buttonView struct {
	Name    string
	Label   string
	Current settings.Action
}

func buttonViews(current settings.Settings) []buttonView {
	views := make([]buttonView, len(scanner.FrontButtons))
	for index, button := range scanner.FrontButtons {
		views[index] = buttonView{Name: button.Name, Label: button.Label, Current: current.Buttons[settings.Button(button.Name)]}
	}
	return views
}
