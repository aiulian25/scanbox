package clock

import (
	"testing"
	"time"
)

// Whatever is typed, a date that is read back is a whole day, and reading the day as Scan
// Box writes it, day first, gives the same day: nothing typed is read month first.
func FuzzParseDateReadsWhatItWrites(f *testing.F) {
	for _, typed := range []string{"11/11/2026", "11.11.2026", "11-11-2026", "2026-11-11", "11 Nov 2026", "November 11, 2026",
		"31/04/2026", "29/02/2028", "1/2/0003", " 3 . 4 . 2026 ", "", "12/31/2026"} {
		f.Add(typed)
	}
	clock := New(DefaultTimeZone, DefaultDateStyle, DefaultTimeStyle)
	f.Fuzz(func(t *testing.T, typed string) {
		parsed, err := clock.ParseDate(typed)
		if err != nil {
			return
		}
		if !parsed.Equal(clock.StartOfDay(parsed)) {
			t.Fatalf("ParseDate(%q) = %v, not the start of a day", typed, parsed)
		}
		written := parsed.Format(time.DateOnly)
		again, err := clock.ParseDate(parsed.Format("2/1/2006"))
		if err != nil || !again.Equal(parsed) {
			t.Fatalf("ParseDate(%q) = %s, but reading that day back day first gave %v, %v", typed, written, again, err)
		}
	})
}
