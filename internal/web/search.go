package web

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"scanbox/internal/clock"
	"scanbox/internal/station"
)

const (
	anyTime    = ""
	today      = "today"
	lastWeek   = "week"
	thisMonth  = "month"
	lastMonth  = "lastmonth"
	thisYear   = "year"
	daysInWeek = 7
)

type choice struct {
	Value string
	Label string
}

var periodChoices = []choice{
	{anyTime, "Any time"}, {today, "Today"}, {lastWeek, "Last 7 days"},
	{thisMonth, "This month"}, {lastMonth, "Last month"}, {thisYear, "This year"},
}

var stateChoices = []choice{
	{"", "Any"}, {string(station.ToReview), "To review"}, {string(station.Waiting), "Waiting"}, {string(station.Submitted), "Accepted"},
	{string(station.Sent), "In Paperless"}, {string(station.Failed), "Failed"},
}

type monthView struct {
	Label   string
	Param   string
	Count   int
	Current bool
}

type dayView struct {
	Heading   string
	Documents []documentView
}

type searchView struct {
	Text     string
	Period   string
	From     string
	To       string
	State    string
	Month    string
	Day      string
	Periods  []choice
	States   []choice
	Months   []monthView
	Days     []dayView
	Count    int
	Problem  string
	Filtered bool
	Example  string
}

// search reads the Scans page's form, finds the scans and groups them by day.
func (s *Server) search(request *http.Request) searchView {
	form := request.URL.Query()
	view := searchView{
		Text: strings.TrimSpace(form.Get("q")), Period: form.Get("period"), From: form.Get("from"), To: form.Get("to"),
		State: form.Get("state"), Month: form.Get("month"), Day: form.Get("day"), Periods: periodChoices, States: stateChoices,
	}
	view.Filtered = view.Text != "" || view.Period != "" || view.From != "" || view.To != "" || view.State != "" || view.Month != "" || view.Day != ""
	current := s.options.Settings.Get()
	writer := current.Clock()
	view.Example = writer.Date(s.options.Now())
	query := station.Query{Text: view.Text, State: station.State(view.State), Clock: writer}
	query.From, query.Until, view.Problem = period(form, s.options.Now(), writer)
	found := s.options.Station.Search(query)
	view.Count = len(found)
	for _, document := range found {
		heading := writer.Heading(document.Created)
		if len(view.Days) == 0 || view.Days[len(view.Days)-1].Heading != heading {
			view.Days = append(view.Days, dayView{Heading: heading})
		}
		last := &view.Days[len(view.Days)-1]
		last.Documents = append(last.Documents, describeDocument(document, current.PaperlessURL, s.canSend(current), s.options.Now(), writer))
	}
	for _, month := range s.options.Station.Months() {
		param := month.Start.Format(monthParamLayout)
		view.Months = append(view.Months, monthView{Label: writer.Month(month.Start), Param: param, Count: month.Count, Current: param == view.Month})
	}
	return view
}

// period turns a day or month from the archive, typed dates or a named period into a
// time range. Typed dates win over the named period; the archive's choice wins over both.
func period(form url.Values, now time.Time, writer clock.Clock) (time.Time, time.Time, string) {
	if day := form.Get("day"); day != "" {
		start, err := time.ParseInLocation(dayParamLayout, day, writer.Location)
		if err != nil {
			return time.Time{}, time.Time{}, "That day isn't known."
		}
		return start, start.AddDate(0, 0, 1), ""
	}
	if month := form.Get("month"); month != "" {
		start, err := time.ParseInLocation(monthParamLayout, month, writer.Location)
		if err != nil {
			return time.Time{}, time.Time{}, "That month isn't known."
		}
		return start, start.AddDate(0, 1, 0), ""
	}
	from, until, problem := typedRange(form.Get("from"), form.Get("to"), writer, now)
	if problem != "" || !from.IsZero() || !until.IsZero() {
		return from, until, problem
	}
	day := writer.StartOfDay(now)
	firstOfMonth := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, writer.Location)
	switch form.Get("period") {
	case today:
		return day, day.AddDate(0, 0, 1), ""
	case lastWeek:
		return day.AddDate(0, 0, 1-daysInWeek), day.AddDate(0, 0, 1), ""
	case thisMonth:
		return firstOfMonth, firstOfMonth.AddDate(0, 1, 0), ""
	case lastMonth:
		return firstOfMonth.AddDate(0, -1, 0), firstOfMonth, ""
	case thisYear:
		first := time.Date(day.Year(), time.January, 1, 0, 0, 0, 0, writer.Location)
		return first, first.AddDate(1, 0, 0), ""
	}
	return time.Time{}, time.Time{}, ""
}

func typedRange(fromText, toText string, writer clock.Clock, now time.Time) (time.Time, time.Time, string) {
	advice := "Write dates day first, for example " + writer.Date(now) + "."
	var from, until time.Time
	if strings.TrimSpace(fromText) != "" {
		parsed, err := writer.ParseDate(fromText)
		if err != nil {
			return time.Time{}, time.Time{}, advice
		}
		from = parsed
	}
	if strings.TrimSpace(toText) != "" {
		parsed, err := writer.ParseDate(toText)
		if err != nil {
			return time.Time{}, time.Time{}, advice
		}
		until = parsed.AddDate(0, 0, 1)
	}
	return from, until, ""
}
