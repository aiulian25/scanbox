//go:build sane

package scanner

import (
	"context"
	"errors"
	"image"
	"os"
	"path/filepath"
	"testing"
)

const (
	// saneTestVariable is set where SANE's test backend is installed, as in the image build.
	saneTestVariable = "SCANBOX_SANE_TEST_BACKEND"
	saneConfigFolder = "SANE_CONFIG_DIR"
	testBackend      = "test"
)

// useBackends points SANE at a settings folder that loads only the given backends.
func useBackends(t *testing.T, backends string) {
	t.Helper()
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, "dll.conf"), []byte(backends), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(saneConfigFolder, folder)
}

// SANE's test backend stands in for a scanner, so the real driver code opens a device,
// sets its options and reads its pictures, in colour and in grey.
func TestTheSANEDriverScansFromSANEsTestBackend(t *testing.T) {
	if os.Getenv(saneTestVariable) == "" {
		t.Skipf("SANE's test backend isn't installed here; the image build sets %s and runs this", saneTestVariable)
	}
	useBackends(t, testBackend+"\n")
	device, err := Default().Open()
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer device.Close()
	if details := device.Details(); details.Driver != testBackend || details.Library == "" || details.Model == "" {
		t.Fatalf("Details = %+v", details)
	}
	// Two inches by one: whole numbers of pixels at any resolution.
	area := Area{WidthMM: 2 * millimetresPerInch, HeightMM: millimetresPerInch}
	cases := map[Mode]func(image.Image) bool{
		Grey:   func(picture image.Image) bool { _, isGrey := picture.(*image.Gray); return isGrey },
		Colour: func(picture image.Image) bool { _, isColour := picture.(*image.RGBA); return isColour },
	}
	for mode, rightKind := range cases {
		picture, err := device.Scan(context.Background(), Request{Mode: mode, ResolutionDPI: 100, Area: area})
		if err != nil {
			t.Fatalf("Scan in %s: %v", mode, err)
		}
		// The backend rounds the area to its own steps, so a few pixels either way are fine.
		size := picture.Bounds().Size()
		t.Logf("Scan in %s gave a %T of %d by %d pixels", mode, picture, size.X, size.Y)
		if !rightKind(picture) || size.X < 195 || size.X > 205 || size.Y < 95 || size.Y > 105 {
			t.Fatalf("Scan in %s gave a %T of %v, want about 200 by 100 pixels", mode, picture, size)
		}
	}
	whole, err := device.Scan(context.Background(), Request{Mode: Grey, ResolutionDPI: 50})
	if err != nil || whole.Bounds().Dx() <= 0 || whole.Bounds().Dy() <= 0 {
		t.Fatalf("a scan of the whole glass: %v, %v", whole.Bounds(), err)
	}
	if name, pressed, err := device.PressedButton(); pressed || err != nil {
		t.Fatalf("the test backend has no buttons, yet PressedButton = %q, %v, %v", name, pressed, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := device.Scan(cancelled, Request{Mode: Grey, ResolutionDPI: 100, Area: area}); !errors.Is(err, context.Canceled) {
		t.Fatalf("a scan whose context was cancelled: %v", err)
	}
	if _, err := device.Scan(context.Background(), Request{Mode: Grey, ResolutionDPI: 100, Area: area}); err != nil {
		t.Fatalf("a scan after a cancelled one: %v", err)
	}
}

// An unplugged scanner shows as its device file deleted while the driver holds it open,
// even once the scanner is back under a new file. Runs wherever the sane tag builds.
func TestSANEDeviceFileDeletedMeansUnplugged(t *testing.T) {
	folder := t.TempDir() + string(filepath.Separator)
	held, err := os.Create(filepath.Join(folder, "003"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if holdsDeletedFile(folder) {
		t.Fatal("a device file still there counts as unplugged")
	}
	if err := os.Remove(held.Name()); err != nil {
		t.Fatal(err)
	}
	replugged, err := os.Create(filepath.Join(folder, "004"))
	if err != nil {
		t.Fatal(err)
	}
	replugged.Close()
	if !holdsDeletedFile(folder) {
		t.Fatal("the deleted device file held open wasn't noticed, though a new one is there")
	}
	held.Close()
	if holdsDeletedFile(folder) {
		t.Fatal("once the old file is let go, nothing should count as unplugged")
	}
}

func TestWithoutAScannerSANESaysSo(t *testing.T) {
	if os.Getenv(saneTestVariable) == "" {
		t.Skipf("SANE isn't set up for tests here; the image build sets %s and runs this", saneTestVariable)
	}
	useBackends(t, "")
	if _, err := Default().Open(); !errors.Is(err, ErrNoScanner) {
		t.Fatalf("Open with no backend = %v, want ErrNoScanner", err)
	}
}
