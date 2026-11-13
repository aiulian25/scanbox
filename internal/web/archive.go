package web

import (
	"strings"
	"time"

	"scanbox/internal/station"
)

const (
	// dayParamLayout names a day in links: 11-11-2026, day first.
	dayParamLayout = "02-01-2006"
	yearLayout     = "2006"
)

// archiveView is the scan archive beside the page: years and months, each with how many
// scans it holds. Only the month on show lists its days and their scans; every other month
// is a link that shows it, so the page stays small however many scans are kept.
type archiveView struct {
	Count int
	Years []archiveYear
	Usage string
	// Damaged names the scans that couldn't be read at the start, and where their files are.
	Damaged string
}

type archiveYear struct {
	Label  string
	Count  int
	Open   bool
	Months []archiveMonth
}

type archiveMonth struct {
	Label   string
	Param   string
	Count   int
	Open    bool
	Current bool
	Days    []archiveDay
}

type archiveDay struct {
	Label     string
	Param     string
	Count     int
	Open      bool
	Current   bool
	Today     bool
	Documents []documentView
}

// archive groups every scan by year, month and day. The month on show is the one
// chosen on the Scans page, or the newest; its chosen or newest day is open.
func (s *Server) archive(chosenMonth, chosenDay string) archiveView {
	current := s.options.Settings.Get()
	writer := current.Clock()
	now := s.options.Now()
	documents := s.options.Station.Search(station.Query{Clock: writer})
	used, free := s.options.Station.Usage()
	view := archiveView{Count: len(documents), Usage: readableSize(used) + " used · " + readableSize(free) + " free",
		Damaged: describeDamaged(s.options.Station.Damaged())}
	shownMonth := chosenMonth
	if day, err := time.ParseInLocation(dayParamLayout, chosenDay, writer.Location); err == nil {
		shownMonth = day.Format(monthParamLayout)
	}
	if shownMonth == "" && len(documents) > 0 {
		shownMonth = documents[0].Created.In(writer.Location).Format(monthParamLayout)
	}
	for _, document := range documents {
		local := document.Created.In(writer.Location)
		year := countInYear(&view, local)
		month := countInMonth(year, local, shownMonth, chosenMonth)
		if !month.Open {
			continue
		}
		day := countInDay(month, local, chosenDay, writer.SameDay(local, now), writer.ShortDay(local))
		day.Documents = append(day.Documents, describeDocument(document, current.PaperlessURL, s.canSend(current), now, writer))
	}
	openFirstDay(&view, chosenDay)
	return view
}

// describeDamaged says which scan folders couldn't be read, for the foot of the archive.
func describeDamaged(folders []string) string {
	if len(folders) == 0 {
		return ""
	}
	return countWords(len(folders), "scan couldn't be read and isn't listed", "scans couldn't be read and aren't listed") +
		". The files are kept in " + strings.Join(folders, ", ") + "; the log says why."
}

func countInYear(view *archiveView, local time.Time) *archiveYear {
	label := local.Format(yearLayout)
	if len(view.Years) == 0 || view.Years[len(view.Years)-1].Label != label {
		view.Years = append(view.Years, archiveYear{Label: label})
	}
	year := &view.Years[len(view.Years)-1]
	year.Count++
	return year
}

func countInMonth(year *archiveYear, local time.Time, shownMonth, chosenMonth string) *archiveMonth {
	param := local.Format(monthParamLayout)
	if len(year.Months) == 0 || year.Months[len(year.Months)-1].Param != param {
		year.Months = append(year.Months, archiveMonth{Label: local.Month().String(), Param: param, Open: param == shownMonth, Current: param == chosenMonth})
	}
	month := &year.Months[len(year.Months)-1]
	month.Count++
	if month.Open {
		year.Open = true
	}
	return month
}

func countInDay(month *archiveMonth, local time.Time, chosenDay string, today bool, label string) *archiveDay {
	param := local.Format(dayParamLayout)
	if len(month.Days) == 0 || month.Days[len(month.Days)-1].Param != param {
		month.Days = append(month.Days, archiveDay{Label: label, Param: param, Current: param == chosenDay, Today: today})
	}
	day := &month.Days[len(month.Days)-1]
	day.Count++
	return day
}

// openFirstDay opens the chosen day, or else the newest day of the month on show.
func openFirstDay(view *archiveView, chosenDay string) {
	for yearIndex := range view.Years {
		for monthIndex := range view.Years[yearIndex].Months {
			month := &view.Years[yearIndex].Months[monthIndex]
			if !month.Open || len(month.Days) == 0 {
				continue
			}
			for dayIndex := range month.Days {
				month.Days[dayIndex].Open = month.Days[dayIndex].Current
			}
			if chosenDay == "" {
				month.Days[0].Open = true
			}
		}
	}
}
