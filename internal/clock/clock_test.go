package clock

import (
	"testing"
	"time"
)

var sample = time.Date(2026, 4, 3, 21, 5, 9, 0, time.UTC)

// The defaults write dates day first and time in 24 hours, in UTC until a zone is chosen.
func TestDefaultsAreDayFirstAnd24HourInUTC(t *testing.T) {
	clock := New("", "", "")
	if clock.Location.String() != DefaultTimeZone || DefaultTimeZone != "UTC" {
		t.Fatalf("zone %s", clock.Location)
	}
	if got := clock.DateTime(sample); got != "03/04/2026 21:05" {
		t.Fatalf("default date and time %q", got)
	}
	if got := clock.Heading(sample); got != "Friday 3 April 2026" {
		t.Fatalf("heading %q", got)
	}
}

func TestEveryStyle(t *testing.T) {
	want := map[string]string{
		"dmy-slash": "03/04/2026", "dmy-dot": "03.04.2026", "dmy-dash": "03-04-2026", "d-mon-y": "3 Apr 2026",
		"d-month-y": "3 April 2026", "wd-d-mon-y": "Fri 3 Apr 2026", "mon-d-y": "Apr 3 2026", "iso": "2026-04-03",
	}
	for _, style := range DateStyles {
		if got := New("UTC", style.ID, "").Date(sample); got != want[style.ID] {
			t.Errorf("%s gave %q, want %q", style.ID, got, want[style.ID])
		}
	}
	times := map[string]string{"24": "21:05", "24-dot": "21.05", "24-seconds": "21:05:09", "12": "9:05 pm"}
	for _, style := range TimeStyles {
		if got := New("UTC", "", style.ID).Time(sample); got != times[style.ID] {
			t.Errorf("%s gave %q, want %q", style.ID, got, times[style.ID])
		}
	}
}

func TestNamesNeverHoldSlashesOrColons(t *testing.T) {
	date, clockTime := New("UTC", "dmy-slash", "24").ForName(sample)
	if date != "03-04-2026" || clockTime != "21-05" {
		t.Fatalf("name parts %q %q", date, clockTime)
	}
}

func TestTypedDatesAreNeverMonthFirst(t *testing.T) {
	clock := New("UTC", "mon-d-y", "")
	for _, typed := range []string{"03/04/2026", "3.4.2026", "3-4-2026", "2026-04-03", "3 Apr 2026", "Apr 3 2026", "April 3, 2026"} {
		parsed, err := clock.ParseDate(typed)
		if err != nil || parsed.Day() != 3 || parsed.Month() != time.April {
			t.Errorf("%q read as %v %v", typed, parsed, err)
		}
	}
	if _, err := clock.ParseDate("04/31/2026"); err == nil {
		t.Fatal("a month-first date was accepted")
	}
}

func TestUnknownZoneFallsBack(t *testing.T) {
	if _, err := LoadZone("Mars/Olympus"); err == nil {
		t.Fatal("an unknown zone was accepted")
	}
	if New("Mars/Olympus", "", "").Location.String() != DefaultTimeZone {
		t.Fatal("an unknown zone didn't fall back to the default")
	}
}
