package station

import (
	"errors"
	"fmt"
	"log"
	"time"
)

const (
	// scannerStillLimit is how long the scanner loop may go without a beat while it isn't
	// scanning: it beats several times a second.
	scannerStillLimit = time.Minute
	// uploadsStillRounds is how many upload rounds may pass without a beat while no upload
	// is under way; asking the destination about a document is bounded by its own timeouts.
	uploadsStillRounds = 5
	// deadlinePer300DPI is how long a page may take at 300 dpi; it grows with the resolution.
	deadlinePer300DPI = 2 * time.Minute
	referenceDPI      = 300
	// defaultStuckGrace is how long past its deadline a scan may go on before the driver
	// counts as stuck.
	defaultStuckGrace = time.Minute
)

var (
	errScannerStuck = errors.New("a scan has gone on far past its deadline: the scanner driver doesn't return")
	errScannerStill = errors.New("the scanner loop has stood still")
	errUploadsStill = errors.New("the upload loop has stood still")
)

// beats are when each loop last showed it was moving.
type beats struct {
	scanner time.Time
	uploads time.Time
	// uploading says an upload is under way; it ends by itself when it stands still.
	uploading bool
	// scanStarted and scanLimit say when the scan under way started, and how long it may take.
	scanStarted time.Time
	scanLimit   time.Duration
}

func (s *Station) beatScanner() {
	now := s.config.Now()
	s.mutex.Lock()
	s.beats.scanner = now
	s.mutex.Unlock()
}

func (s *Station) beatUploads() {
	now := s.config.Now()
	s.mutex.Lock()
	s.beats.uploads = now
	s.mutex.Unlock()
}

// sendingNow says which document is being uploaded, "" when none is: the upload loop is
// moving, and that document can't be deleted.
func (s *Station) sendingNow(id string) {
	now := s.config.Now()
	s.setState(func() { s.sending, s.beats.uploading, s.beats.uploads = id, id != "", now })
}

// Health says whether both loops are moving: the scanner loop, which also reads the
// buttons, and the upload loop. A scan may take its deadline and the grace after it.
func (s *Station) Health() error {
	now := s.config.Now()
	s.mutex.Lock()
	defer s.mutex.Unlock()
	beats := s.beats
	if s.scanning && now.After(beats.scanStarted.Add(beats.scanLimit+s.config.StuckGrace)) {
		return errScannerStuck
	}
	if !s.scanning && now.Sub(beats.scanner) > scannerStillLimit {
		return fmt.Errorf("%w since %s", errScannerStill, beats.scanner.Format(time.TimeOnly))
	}
	if !beats.uploading && now.Sub(beats.uploads) > uploadsStillRounds*s.config.Retry {
		return fmt.Errorf("%w since %s", errUploadsStill, beats.uploads.Format(time.TimeOnly))
	}
	return nil
}

// scanDeadline is how long a page at this resolution may take: two minutes at 300 dpi,
// in proportion above and below, unless the configuration fixes it.
func (s *Station) scanDeadline(resolution int) time.Duration {
	if s.config.ScanDeadline != 0 {
		return s.config.ScanDeadline
	}
	return deadlinePer300DPI * time.Duration(max(resolution, 1)) / referenceDPI
}

// watchScan starts the scan's clock, and a watchdog for a driver that never returns: past
// the deadline and the grace it logs, and calls Config.Stuck, which in the program ends
// it so that Docker starts it again. The returned function stops both.
func (s *Station) watchScan(deadline time.Duration) func() {
	started := s.config.Now()
	s.setState(func() { s.scanning, s.scanError, s.beats.scanStarted, s.beats.scanLimit = true, "", started, deadline })
	watchdog := time.AfterFunc(deadline+s.config.StuckGrace, func() {
		log.Printf("scan: %v (started %s, deadline %v)", errScannerStuck, started.Format(time.TimeOnly), deadline)
		if s.config.Stuck != nil {
			s.config.Stuck()
		}
	})
	return func() {
		watchdog.Stop()
		s.setState(func() { s.scanning = false })
		s.beatScanner()
	}
}
