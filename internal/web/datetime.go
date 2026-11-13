package web

import (
	"time"

	"scanbox/internal/clock"
	"scanbox/internal/settings"
)

const olderScanDays = 3

// europeanZones come first in the time-zone menu; the browser adds every other zone.
var europeanZones = []string{
	"Europe/London", "Europe/Dublin", "Europe/Lisbon", "Europe/Paris", "Europe/Brussels", "Europe/Amsterdam",
	"Europe/Berlin", "Europe/Madrid", "Europe/Rome", "Europe/Vienna", "Europe/Zurich", "Europe/Copenhagen",
	"Europe/Oslo", "Europe/Stockholm", "Europe/Helsinki", "Europe/Warsaw", "Europe/Prague", "Europe/Budapest",
	"Europe/Bucharest", "Europe/Sofia", "Europe/Athens", "Europe/Istanbul", "Europe/Kyiv",
}

type option struct {
	Value    string `json:"value"`
	Label    string `json:"label"`
	Selected bool   `json:"selected"`
}

// dateTimeView is the Date and time section: menus whose options are examples
// from right now, and a preview of where dates appear.
type dateTimeView struct {
	Zones    []option `json:"-"`
	Dates    []option `json:"dates"`
	Times    []option `json:"times"`
	ZoneNote string   `json:"zoneNote"`
	Heading  string   `json:"heading"`
	Row      string   `json:"row"`
	Recent   string   `json:"recent"`
	Name     string   `json:"name"`
}

func describeDateTime(current settings.Settings, now time.Time) dateTimeView {
	writer := current.Clock()
	view := dateTimeView{
		ZoneNote: "It is " + writer.Time(now) + " on " + writer.Date(now) + " there now.",
		Heading:  writer.Heading(now),
		Row:      "Gas bill November · 2 pages · colour · " + writer.Time(now),
		Recent:   "Council tax · 1 page · greyscale · " + writer.DateTime(now.AddDate(0, 0, -olderScanDays)),
		Name:     current.DocumentName(now),
	}
	zones := europeanZones
	if !contains(zones, current.TimeZone) {
		zones = append([]string{current.TimeZone}, zones...)
	}
	for _, zone := range zones {
		location, err := clock.LoadZone(zone)
		if err != nil {
			continue
		}
		view.Zones = append(view.Zones, option{Value: zone, Label: zone + " (now " + now.In(location).Format("15:04") + ")", Selected: zone == current.TimeZone})
	}
	for _, style := range clock.DateStyles {
		view.Dates = append(view.Dates, option{Value: style.ID, Label: clock.Example(style, now, writer.Location), Selected: style.ID == current.DateStyle})
	}
	for _, style := range clock.TimeStyles {
		view.Times = append(view.Times, option{Value: style.ID, Label: clock.Example(style, now, writer.Location), Selected: style.ID == current.TimeStyle})
	}
	return view
}

func contains(list []string, wanted string) bool {
	for _, item := range list {
		if item == wanted {
			return true
		}
	}
	return false
}
