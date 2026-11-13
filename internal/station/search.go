package station

import (
	"sort"
	"strings"
	"time"

	"scanbox/internal/clock"
)

// Query finds scans. Every word of Text must appear in the name or in the date,
// spelt as the Clock writes it or as people type dates.
type Query struct {
	Text  string
	From  time.Time
	Until time.Time
	State State
	Clock clock.Clock
}

// Month is a date folder: a month and how many scans it holds.
type Month struct {
	Start time.Time
	Count int
}

func (q Query) matches(document Document) bool {
	if q.State != "" && document.State != q.State {
		return false
	}
	if !q.From.IsZero() && document.Created.Before(q.From) {
		return false
	}
	if !q.Until.IsZero() && !document.Created.Before(q.Until) {
		return false
	}
	words := strings.Fields(strings.ToLower(q.Text))
	if len(words) == 0 {
		return true
	}
	searchable := strings.ToLower(document.Name + " " + q.Clock.Searchable(document.Created))
	for _, word := range words {
		if !strings.Contains(searchable, word) {
			return false
		}
	}
	return true
}

// Search gives the matching scans, newest first.
func (s *Station) Search(query Query) []Document {
	s.mutex.Lock()
	sending := s.sending
	s.mutex.Unlock()
	s.files.Lock()
	all := markSending(s.disk.list(""), sending)
	s.files.Unlock()
	found := make([]Document, 0, len(all))
	for _, document := range all {
		if query.matches(document) {
			found = append(found, document)
		}
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Created.After(found[j].Created) })
	return found
}

// Months lists the months that hold scans, in the chosen time zone, newest first.
func (s *Station) Months() []Month {
	location := s.config.Settings.Get().Clock().Location
	s.files.Lock()
	all := s.disk.list("")
	s.files.Unlock()
	counts := map[time.Time]int{}
	for _, document := range all {
		local := document.Created.In(location)
		counts[time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, location)]++
	}
	months := make([]Month, 0, len(counts))
	for start, count := range counts {
		months = append(months, Month{Start: start, Count: count})
	}
	sort.Slice(months, func(i, j int) bool { return months[i].Start.After(months[j].Start) })
	return months
}
