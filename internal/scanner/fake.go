package scanner

import (
	"context"
	"errors"
	"image"
	"image/color"
	"sync"
	"time"
)

const (
	millimetresPerInch = 25.4
	fakeLineSpacingMM  = 8
	fakeMarginMM       = 20
	glassWidthMM       = 216
	glassHeightMM      = 297
)

// Fake is a scanner in memory, for tests and for trying the page without hardware.
type Fake struct {
	mutex     sync.Mutex
	missing   bool
	failScans bool
	busy      bool
	// delay makes a scan take that long, unless it is stopped first.
	delay time.Duration
	// hung, while set, holds every scan until it is closed, whatever is asked of it.
	hung    chan struct{}
	presses []string
	scans   int
}

// SetBusy makes every scan meet a scanner another program is using, or not.
func (f *Fake) SetBusy(busy bool) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.busy = busy
}

// SetDelay makes every scan take this long, unless it is stopped first.
func (f *Fake) SetDelay(delay time.Duration) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.delay = delay
}

// Hang makes scans hold still, as a driver that never returns would, until the returned
// function is called.
func (f *Fake) Hang() (release func()) {
	hung := make(chan struct{})
	f.mutex.Lock()
	f.hung = hung
	f.mutex.Unlock()
	return func() {
		f.mutex.Lock()
		f.hung = nil
		f.mutex.Unlock()
		close(hung)
	}
}

// SetMissing unplugs the fake scanner, or plugs it back in.
func (f *Fake) SetMissing(missing bool) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.missing = missing
}

// SetFailing makes every scan fail, or work again.
func (f *Fake) SetFailing(failing bool) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.failScans = failing
}

// ScanCount says how many scans were asked for.
func (f *Fake) ScanCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.scans
}

func NewFake() *Fake { return &Fake{} }

// Press queues a front-button press, as if someone pressed it.
func (f *Fake) Press(button string) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.presses = append(f.presses, button)
}

func (f *Fake) Open() (Device, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.missing {
		return nil, ErrNoScanner
	}
	return fakeDevice{fake: f}, nil
}

type fakeDevice struct {
	fake *Fake
}

func (fakeDevice) Name() string { return "Test scanner" }

func (d fakeDevice) Details() Details {
	return Details{Model: d.Name(), Kind: "pretend flatbed scanner", Driver: "fake", Library: "no scanning library"}
}

func (d fakeDevice) Scan(ctx context.Context, request Request) (image.Image, error) {
	d.fake.mutex.Lock()
	failing, missing, busy, delay, hung := d.fake.failScans, d.fake.missing, d.fake.busy, d.fake.delay, d.fake.hung
	d.fake.scans++
	d.fake.mutex.Unlock()
	if hung != nil {
		<-hung
	}
	if missing {
		return nil, ErrNoScanner
	}
	if busy {
		return nil, ErrBusy
	}
	if failing {
		return nil, errors.New("the scanner stopped during the scan")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(delay):
	}
	return fakePage(request), nil
}

func (d fakeDevice) PressedButton() (string, bool, error) {
	d.fake.mutex.Lock()
	defer d.fake.mutex.Unlock()
	if d.fake.missing {
		return "", false, ErrNoScanner
	}
	if len(d.fake.presses) == 0 {
		return "", false, nil
	}
	button := d.fake.presses[0]
	d.fake.presses = d.fake.presses[1:]
	return button, true, nil
}

func (fakeDevice) Close() error { return nil }

// fakePage draws a white page with grey lines where text would be.
func fakePage(request Request) image.Image {
	widthMM, heightMM := request.Area.WidthMM, request.Area.HeightMM
	if widthMM == 0 {
		widthMM, heightMM = glassWidthMM, glassHeightMM
	}
	pixels := func(millimetres float64) int {
		return int(millimetres / millimetresPerInch * float64(request.ResolutionDPI))
	}
	bounds := image.Rect(0, 0, pixels(widthMM), pixels(heightMM))
	lineEvery, margin := pixels(fakeLineSpacingMM), pixels(fakeMarginMM)
	isText := func(x, y int) bool {
		return x > margin && x < bounds.Dx()-margin && y > margin && y%lineEvery < lineEvery/4
	}
	if request.Mode == Grey {
		page := image.NewGray(bounds)
		for y := 0; y < bounds.Dy(); y++ {
			for x := 0; x < bounds.Dx(); x++ {
				page.Pix[page.PixOffset(x, y)] = greyLevel(isText(x, y))
			}
		}
		return page
	}
	page := image.NewRGBA(bounds)
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			level := greyLevel(isText(x, y))
			page.SetRGBA(x, y, color.RGBA{R: level, G: level, B: level + (255-level)/8, A: 255})
		}
	}
	return page
}

func greyLevel(text bool) uint8 {
	if text {
		return 90
	}
	return 250
}
