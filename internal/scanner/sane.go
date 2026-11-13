//go:build sane

package scanner

/*
#cgo LDFLAGS: -lsane
#include <stdlib.h>
#include <string.h>
#include <sane/sane.h>

static SANE_Word to_fixed(double value) { return SANE_FIX(value); }

static double range_maximum(const SANE_Option_Descriptor *option) {
	if (option->constraint_type != SANE_CONSTRAINT_RANGE || option->constraint.range == NULL) return -1;
	if (option->type == SANE_TYPE_FIXED) return SANE_UNFIX(option->constraint.range->max);
	return option->constraint.range->max;
}

static const SANE_Device *device_at(const SANE_Device **list, int index) { return list[index]; }

static int version_major(SANE_Int code) { return SANE_VERSION_MAJOR(code); }
static int version_minor(SANE_Int code) { return SANE_VERSION_MINOR(code); }
static int version_build(SANE_Int code) { return SANE_VERSION_BUILD(code); }
*/
import "C"

import (
	"context"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
	"unsafe"
)

const (
	readBufferBytes  = 64 * 1024
	supportedDepth   = 8
	saneColourMode   = "Color"
	saneGreyMode     = "Gray"
	optionMode       = "mode"
	optionResolution = "resolution"
	optionLeft       = "tl-x"
	optionTop        = "tl-y"
	optionRight      = "br-x"
	optionBottom     = "br-y"
	optionButton1    = "button-1"
	optionButton2    = "button-2"
	optionTarget     = "target"
	// usbDeviceFolder holds the USB device files; an open one whose file was deleted ends in
	// deletedMark among the process's own files.
	usbDeviceFolder = "/dev/bus/usb/"
	deletedMark     = " (deleted)"
	ownFiles        = "/proc/self/fd"
)

// Default is the real scanner driver, through the SANE library.
func Default() Driver { return saneDriver{} }

type saneDriver struct{}

// Open starts SANE afresh each time, so a scanner plugged in later is found.
func (saneDriver) Open() (Device, error) {
	var versionCode C.SANE_Int
	if status := C.sane_init(&versionCode, nil); status != C.SANE_STATUS_GOOD {
		return nil, statusError("starting SANE", status)
	}
	name, details, err := firstDevice()
	if err != nil {
		C.sane_exit()
		return nil, err
	}
	details.Library = fmt.Sprintf("SANE %d.%d.%d", int(C.version_major(versionCode)), int(C.version_minor(versionCode)), int(C.version_build(versionCode)))
	device := &saneDevice{deviceName: name, details: details}
	if err := device.open(); err != nil {
		C.sane_exit()
		return nil, err
	}
	return device, nil
}

// firstDevice gives the first scanner's SANE name, and what it says about itself. The
// name starts with the driver's, as in "pixma:04A91912_4B56CB".
func firstDevice() (string, Details, error) {
	var list **C.SANE_Device
	if status := C.sane_get_devices(&list, C.SANE_TRUE); status != C.SANE_STATUS_GOOD {
		return "", Details{}, statusError("looking for scanners", status)
	}
	first := C.device_at(list, 0)
	if first == nil {
		return "", Details{}, ErrNoScanner
	}
	name := C.GoString(first.name)
	driver, _, _ := strings.Cut(name, ":")
	return name, Details{
		Model:  strings.TrimSpace(C.GoString(first.vendor) + " " + C.GoString(first.model)),
		Kind:   C.GoString(first._type),
		Driver: driver,
	}, nil
}

type saneOption struct {
	index      C.SANE_Int
	descriptor *C.SANE_Option_Descriptor
}

type saneDevice struct {
	deviceName string
	details    Details
	handle     C.SANE_Handle
	options    map[string]saneOption
}

func (d *saneDevice) Name() string { return d.details.Model }

func (d *saneDevice) Details() Details { return d.details }

func (d *saneDevice) open() error {
	name := C.CString(d.deviceName)
	defer C.free(unsafe.Pointer(name))
	if status := C.sane_open(name, &d.handle); status != C.SANE_STATUS_GOOD {
		return statusError("opening the scanner", status)
	}
	return d.loadOptions()
}

// loadOptions reads the scanner's options by name. SANE asks for this again after a change
// that may have changed the others, as a new mode can: their descriptions are read anew.
func (d *saneDevice) loadOptions() error {
	d.options = map[string]saneOption{}
	var count C.SANE_Int
	if status := C.sane_control_option(d.handle, 0, C.SANE_ACTION_GET_VALUE, unsafe.Pointer(&count), nil); status != C.SANE_STATUS_GOOD {
		return statusError("reading the scanner's options", status)
	}
	for index := C.SANE_Int(1); index < count; index++ {
		descriptor := C.sane_get_option_descriptor(d.handle, index)
		if descriptor == nil || descriptor.name == nil {
			continue
		}
		d.options[C.GoString(descriptor.name)] = saneOption{index: index, descriptor: descriptor}
	}
	return nil
}

func (d *saneDevice) Close() error {
	C.sane_close(d.handle)
	C.sane_exit()
	return nil
}

// reopen gives a fresh handle. The pixma driver keeps a pressed button set until then.
func (d *saneDevice) reopen() error {
	C.sane_close(d.handle)
	return d.open()
}

func (d *saneDevice) setString(name, value string) error {
	option, found := d.options[name]
	if !found {
		return fmt.Errorf("the scanner has no %q setting", name)
	}
	buffer := C.malloc(C.size_t(option.descriptor.size))
	defer C.free(buffer)
	text := C.CString(value)
	defer C.free(unsafe.Pointer(text))
	C.strncpy((*C.char)(buffer), text, C.size_t(option.descriptor.size))
	return d.control(name, option, C.SANE_ACTION_SET_VALUE, buffer)
}

func (d *saneDevice) setNumber(name string, value float64) error {
	option, found := d.options[name]
	if !found {
		return fmt.Errorf("the scanner has no %q setting", name)
	}
	word := C.SANE_Word(value)
	if option.descriptor._type == C.SANE_TYPE_FIXED {
		word = C.to_fixed(C.double(value))
	}
	return d.control(name, option, C.SANE_ACTION_SET_VALUE, unsafe.Pointer(&word))
}

func (d *saneDevice) number(name string) (int, bool, error) {
	option, found := d.options[name]
	if !found {
		return 0, false, nil
	}
	var word C.SANE_Word
	if err := d.control(name, option, C.SANE_ACTION_GET_VALUE, unsafe.Pointer(&word)); err != nil {
		return 0, true, err
	}
	return int(word), true, nil
}

func (d *saneDevice) control(name string, option saneOption, action C.SANE_Action, value unsafe.Pointer) error {
	var info C.SANE_Int
	if status := C.sane_control_option(d.handle, option.index, action, value, &info); status != C.SANE_STATUS_GOOD {
		return statusError("setting "+name, status)
	}
	if info&C.SANE_INFO_RELOAD_OPTIONS != 0 {
		return d.loadOptions()
	}
	return nil
}

func (d *saneDevice) maximum(name string) float64 {
	option, found := d.options[name]
	if !found {
		return -1
	}
	return float64(C.range_maximum(option.descriptor))
}

// PressedButton also notices a scanner that was unplugged, which the pixma driver's button
// reads don't report: they swallow the error, and would go on reading the scanner that is
// gone even after it is back, under a new device file.
func (d *saneDevice) PressedButton() (string, bool, error) {
	if holdsDeletedFile(usbDeviceFolder) {
		return "", false, fmt.Errorf("%w: it was unplugged", ErrNoScanner)
	}
	first, supported, err := d.number(optionButton1)
	if !supported || err != nil {
		return "", false, err
	}
	second, _, err := d.number(optionButton2)
	if err != nil || (first == 0 && second == 0) {
		return "", false, err
	}
	name := d.buttonName(first)
	return name, true, d.reopen()
}

// holdsDeletedFile says whether this process holds open a file in folder that has been
// deleted since, as the device file of an unplugged scanner is.
func holdsDeletedFile(folder string) bool {
	entries, err := os.ReadDir(ownFiles)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(ownFiles, entry.Name()))
		if err == nil && strings.HasPrefix(target, folder) && strings.HasSuffix(target, deletedMark) {
			return true
		}
	}
	return false
}

func (d *saneDevice) buttonName(first int) string {
	target, hasTarget, _ := d.number(optionTarget)
	if known, isKnown := pixmaTargets[target]; hasTarget && isKnown {
		return known
	}
	if first != 0 {
		return optionButton1
	}
	return optionButton2
}

func (d *saneDevice) Scan(ctx context.Context, request Request) (image.Image, error) {
	if err := d.prepare(request); err != nil {
		return nil, err
	}
	if status := C.sane_start(d.handle); status != C.SANE_STATUS_GOOD {
		return nil, statusError("starting the scan", status)
	}
	defer C.sane_cancel(d.handle)
	var parameters C.SANE_Parameters
	if status := C.sane_get_parameters(d.handle, &parameters); status != C.SANE_STATUS_GOOD {
		return nil, statusError("reading the scan's size", status)
	}
	if parameters.depth != supportedDepth || (parameters.format != C.SANE_FRAME_GRAY && parameters.format != C.SANE_FRAME_RGB) {
		return nil, fmt.Errorf("the scanner sends a picture format Scan Box can't read (depth %d)", int(parameters.depth))
	}
	scan := newPicture(parameters.format == C.SANE_FRAME_RGB, int(parameters.pixels_per_line), int(parameters.bytes_per_line), int(parameters.lines))
	if err := d.read(ctx, scan); err != nil {
		return nil, err
	}
	return scan.Image(), nil
}

func (d *saneDevice) prepare(request Request) error {
	mode := saneGreyMode
	if request.Mode == Colour {
		mode = saneColourMode
	}
	if err := d.setString(optionMode, mode); err != nil {
		return err
	}
	if err := d.setNumber(optionResolution, float64(request.ResolutionDPI)); err != nil {
		return err
	}
	width, height := d.maximum(optionRight), d.maximum(optionBottom)
	if request.Area.WidthMM > 0 && request.Area.WidthMM < width {
		width = request.Area.WidthMM
	}
	if request.Area.HeightMM > 0 && request.Area.HeightMM < height {
		height = request.Area.HeightMM
	}
	for _, setting := range []struct {
		name  string
		value float64
	}{{optionLeft, 0}, {optionTop, 0}, {optionRight, width}, {optionBottom, height}} {
		if err := d.setNumber(setting.name, setting.value); err != nil {
			return err
		}
	}
	return nil
}

// read hands the scan's bytes to scan as they come. One buffer of Go memory is passed to
// SANE for each read; it holds no Go pointers, and SANE keeps none after the call.
func (d *saneDevice) read(ctx context.Context, scan *picture) error {
	buffer := make([]byte, readBufferBytes)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var length C.SANE_Int
		status := C.sane_read(d.handle, (*C.SANE_Byte)(unsafe.Pointer(&buffer[0])), readBufferBytes, &length)
		scan.Write(buffer[:length])
		if status == C.SANE_STATUS_EOF {
			return nil
		}
		if status != C.SANE_STATUS_GOOD {
			return statusError("reading the scan", status)
		}
	}
}

// statusError says what failed and how, in SANE's words, and which of this package's
// errors that is: busy and an unsupported setting are told apart from a lost scanner.
func statusError(doing string, status C.SANE_Status) error {
	message := C.GoString(C.sane_strstatus(status))
	if kind := kindOf(int(status)); kind != nil {
		return fmt.Errorf("%w (%s: %s)", kind, doing, message)
	}
	return fmt.Errorf("%s: %s", doing, message)
}
