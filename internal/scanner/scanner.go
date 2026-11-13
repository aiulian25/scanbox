// Package scanner finds a scanner, scans pages from it and reports presses of its
// front buttons. All calls on a Device must come from one goroutine.
package scanner

import (
	"context"
	"errors"
	"image"
)

type Mode string

const (
	Colour Mode = "colour"
	Grey   Mode = "grey"
)

// Area is the part of the glass to scan, from the top-left corner. Zero means the whole glass.
type Area struct {
	WidthMM  float64
	HeightMM float64
}

type Request struct {
	Mode          Mode
	ResolutionDPI int
	Area          Area
}

// Details are what the scanner and the library driving it say about themselves.
type Details struct {
	// Model is the scanner's make and model.
	Model string
	// Kind is what sort of scanner it reports being, such as "flatbed scanner".
	Kind string
	// Driver is the part of the library that drives this scanner, such as "pixma".
	Driver string
	// Library is the scanning library and its version, such as "SANE 1.4.0".
	Library string
}

type Device interface {
	// Name is the scanner's make and model, for people to read.
	Name() string
	Details() Details
	Scan(ctx context.Context, request Request) (image.Image, error)
	// PressedButton reports a front button pressed since the last call, by its Button name.
	PressedButton() (string, bool, error)
	Close() error
}

// Driver opens the first scanner it can see.
type Driver interface {
	Open() (Device, error)
}

var (
	ErrNoScanner = errors.New("no scanner found")
	// ErrBusy is a scanner another program is using.
	ErrBusy = errors.New("the scanner is busy")
	// ErrUnsupported is a setting the scanner doesn't offer, such as a resolution.
	ErrUnsupported = errors.New("the scanner doesn't offer this setting")
)

// SANE's status codes, which the SANE standard fixes, for the ones that mean more than
// "it didn't work".
const (
	saneStatusDeviceBusy = 3
	saneStatusInvalid    = 4
	saneStatusIOError    = 9
)

// kindOf says which of this package's errors a SANE status means, or nil if none: busy, a
// setting it doesn't offer, or a scanner that stopped answering, as when it is unplugged.
func kindOf(status int) error {
	switch status {
	case saneStatusDeviceBusy:
		return ErrBusy
	case saneStatusInvalid:
		return ErrUnsupported
	case saneStatusIOError:
		return ErrNoScanner
	}
	return nil
}

// ButtonLabel is a front button as printed on the scanner.
type ButtonLabel struct {
	Name  string
	Label string
}

// FrontButtons are the CanoScan LiDE 400's buttons, left to right. The LiDE 300 has the
// same ones without "PDF finish".
var FrontButtons = []ButtonLabel{
	{Name: "pdf", Label: "PDF"},
	{Name: "pdf_finish", Label: "PDF finish"},
	{Name: "auto_scan", Label: "Auto scan"},
	{Name: "copy", Label: "Copy"},
	{Name: "send", Label: "Send"},
}

// pixmaTargets maps SANE's pixma "target" value to the button that sent it.
var pixmaTargets = map[int]string{
	1: "copy",
	2: "auto_scan",
	3: "send",
	5: "pdf",
	6: "pdf_finish",
}
