// Package clock writes dates and times the way people choose in Settings, in their
// time zone, and reads dates they type. A typed date is never read month first.
package clock

import (
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	DefaultTimeZone  = "UTC"
	DefaultDateStyle = "dmy-slash"
	DefaultTimeStyle = "24"
	monthNameLayout  = "January 2006"
	// A date style whose layout starts with the month's short name writes days month first.
	monthFirstPrefix   = "Jan"
	monthFirstShortDay = "Jan 2"
	dayFirstShortDay   = "2 Jan"
)

// Style is one way of writing a date or a time, shown in Settings as an example.
type Style struct {
	ID      string
	Note    string
	layout  string
	heading string
}

var DateStyles = []Style{
	{ID: "dmy-slash", Note: "day/month/year", layout: "02/01/2006", heading: "Monday 2 January 2006"},
	{ID: "dmy-dot", layout: "02.01.2006", heading: "Monday 2 January 2006"},
	{ID: "dmy-dash", layout: "02-01-2006", heading: "Monday 2 January 2006"},
	{ID: "d-mon-y", layout: "2 Jan 2006", heading: "Monday 2 January 2006"},
	{ID: "d-month-y", layout: "2 January 2006", heading: "Monday 2 January 2006"},
	{ID: "wd-d-mon-y", layout: "Mon 2 Jan 2006", heading: "Monday 2 January 2006"},
	{ID: "mon-d-y", Note: "like a desktop clock", layout: "Jan 2 2006", heading: "Monday January 2 2006"},
	{ID: "iso", Note: "year first", layout: "2006-01-02", heading: "Monday 2006-01-02"},
}

var TimeStyles = []Style{
	{ID: "24", Note: "24-hour", layout: "15:04"},
	{ID: "24-dot", Note: "24-hour", layout: "15.04"},
	{ID: "24-seconds", Note: "24-hour, with seconds", layout: "15:04:05"},
	{ID: "12", Note: "12-hour", layout: "3:04 pm"},
}

// typedLayouts are the ways a date may be typed in search. Numbers are always day first;
// a month written as a word can't be mistaken, so it may come first.
var typedLayouts = []string{"2/1/2006", "2006/1/2", "2 Jan 2006", "2 January 2006", "Jan 2 2006", "January 2 2006"}

var ErrUnreadableDate = errors.New("unreadable date")

var (
	zoneCache sync.Map
)

// LoadZone finds a time zone by its name, such as Europe/Paris.
func LoadZone(name string) (*time.Location, error) {
	if cached, found := zoneCache.Load(name); found {
		return cached.(*time.Location), nil
	}
	location, err := time.LoadLocation(name)
	if err != nil || name == "" || name == "Local" {
		return nil, errors.New("unknown time zone")
	}
	zoneCache.Store(name, location)
	return location, nil
}

func ValidDateStyle(id string) bool { return findStyle(DateStyles, id) != nil }
func ValidTimeStyle(id string) bool { return findStyle(TimeStyles, id) != nil }

func findStyle(styles []Style, id string) *Style {
	for index := range styles {
		if styles[index].ID == id {
			return &styles[index]
		}
	}
	return nil
}

// Clock writes dates and times in one zone and one pair of styles.
type Clock struct {
	Location *time.Location
	date     Style
	time     Style
}

// New falls back to the defaults for anything it doesn't know.
func New(zone, dateStyle, timeStyle string) Clock {
	location, err := LoadZone(zone)
	if err != nil {
		location, _ = LoadZone(DefaultTimeZone)
	}
	date, clockTime := findStyle(DateStyles, dateStyle), findStyle(TimeStyles, timeStyle)
	if date == nil {
		date = findStyle(DateStyles, DefaultDateStyle)
	}
	if clockTime == nil {
		clockTime = findStyle(TimeStyles, DefaultTimeStyle)
	}
	return Clock{Location: location, date: *date, time: *clockTime}
}

func (c Clock) Date(moment time.Time) string { return moment.In(c.Location).Format(c.date.layout) }
func (c Clock) Time(moment time.Time) string { return moment.In(c.Location).Format(c.time.layout) }
func (c Clock) DateTime(moment time.Time) string {
	return c.Date(moment) + " " + c.Time(moment)
}

// Heading is a day written out in full, for grouping scans by day.
func (c Clock) Heading(moment time.Time) string { return moment.In(c.Location).Format(c.date.heading) }

// Month names a month, for the month folders on the Scans page.
func (c Clock) Month(moment time.Time) string { return moment.In(c.Location).Format(monthNameLayout) }

// ShortDay writes a day without its year, in the chosen order: "11 Nov", or "Nov 11" for
// a style that puts the month first.
func (c Clock) ShortDay(moment time.Time) string {
	if strings.HasPrefix(c.date.layout, monthFirstPrefix) {
		return moment.In(c.Location).Format(monthFirstShortDay)
	}
	return moment.In(c.Location).Format(dayFirstShortDay)
}

// ForName writes a date and a time without "/" or ":", which names can't hold.
func (c Clock) ForName(moment time.Time) (string, string) {
	safe := strings.NewReplacer("/", "-", ":", "-")
	return safe.Replace(c.Date(moment)), safe.Replace(c.Time(moment))
}

func (c Clock) StartOfDay(moment time.Time) time.Time {
	local := moment.In(c.Location)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, c.Location)
}

func (c Clock) SameDay(first, second time.Time) bool {
	return c.StartOfDay(first).Equal(c.StartOfDay(second))
}

// ParseDate reads a typed date: 11/11/2026, 11.11.2026, 11-11-2026, 2026-11-11,
// 11 Nov 2026 or Nov 11 2026. Numbers are never read month first.
func (c Clock) ParseDate(text string) (time.Time, error) {
	normalised := strings.Join(strings.Fields(strings.NewReplacer("-", "/", ".", "/", ",", " ").Replace(text)), " ")
	for _, layout := range typedLayouts {
		if parsed, err := time.ParseInLocation(layout, normalised, c.Location); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, ErrUnreadableDate
}

// Searchable spells a moment every way people might type it in search.
func (c Clock) Searchable(moment time.Time) string {
	const spellings = "02/01/2006 2/1/2006 02-01-2006 02.01.2006 2006-01-02 2 January 2006 January 2 2006 Monday Jan 15:04 3:04pm"
	return moment.In(c.Location).Format(spellings) + " " + c.DateTime(moment)
}

// Example is how a style writes the given moment, with its note, for the Settings menus.
func Example(style Style, moment time.Time, location *time.Location) string {
	example := moment.In(location).Format(style.layout)
	if style.Note == "" {
		return example
	}
	return example + " (" + style.Note + ")"
}
