package scanner

import (
	"errors"
	"testing"
)

func TestSANEStatusesAreToldApart(t *testing.T) {
	cases := map[int]error{
		saneStatusDeviceBusy: ErrBusy,
		saneStatusInvalid:    ErrUnsupported,
		saneStatusIOError:    ErrNoScanner,
	}
	for status, want := range cases {
		if got := kindOf(status); !errors.Is(got, want) {
			t.Errorf("SANE status %d: %v, want %v", status, got, want)
		}
	}
	// Good, unsupported, cancelled, end of file, jammed and the rest mean nothing more.
	for _, status := range []int{0, 1, 2, 5, 6, 7, 8, 10, 11} {
		if got := kindOf(status); got != nil {
			t.Errorf("SANE status %d: %v, want nothing", status, got)
		}
	}
	if errors.Is(ErrBusy, ErrNoScanner) || errors.Is(ErrUnsupported, ErrNoScanner) {
		t.Fatal("a busy scanner, or a setting it lacks, isn't a missing scanner")
	}
}
