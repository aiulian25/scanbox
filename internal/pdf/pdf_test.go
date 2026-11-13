package pdf

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testDPI = 100

func greyPicture() *image.Gray {
	picture := image.NewGray(image.Rect(0, 0, 827, 1169))
	for index := range picture.Pix {
		picture.Pix[index] = uint8(index % 251)
	}
	return picture
}

func colourPicture() *image.RGBA {
	picture := image.NewRGBA(image.Rect(0, 0, 827, 1169))
	for y := 0; y < 1169; y += 10 {
		for x := 0; x < 827; x++ {
			picture.Set(x, y, color.RGBA{R: 200, G: 30, B: 30, A: 255})
		}
	}
	return picture
}

func TestPagesKeepTheirKind(t *testing.T) {
	colour, err := JPEGPage(colourPicture(), testDPI)
	if err != nil || colour.ColourSpace != RGB || colour.Encoding != JPEG {
		t.Fatalf("colour page: %+v %v", colour, err)
	}
	grey, err := JPEGPage(greyPicture(), testDPI)
	if err != nil || grey.ColourSpace != Grey {
		t.Fatalf("grey JPEG page: %+v %v", grey, err)
	}
	lossless, err := LosslessGreyPage(greyPicture(), testDPI)
	if err != nil || lossless.Encoding != Flate || lossless.Width != 827 {
		t.Fatalf("lossless page: %+v %v", lossless, err)
	}
}

// listed gives pages from a list, one at a time, as Write asks for them.
func listed(pages ...Page) (int, func(int) (Page, error)) {
	return len(pages), func(index int) (Page, error) { return pages[index], nil }
}

func TestPagesAreAskedForOneAtATime(t *testing.T) {
	asked := 0
	held := 0
	failure := errors.New("page 3 can't be read")
	err := Write(io.Discard, Info{}, 5, func(index int) (Page, error) {
		asked++
		if index == 2 {
			return Page{}, failure
		}
		held++
		return Page{Width: 10, Height: 10, DPI: testDPI, ColourSpace: Grey, Encoding: JPEG, Data: []byte("data")}, nil
	})
	if !errors.Is(err, failure) || asked != 3 || held != 2 {
		t.Fatalf("Write asked for %d pages and stopped with %v", asked, err)
	}
}

func TestWriteRefusesAnEmptyDocument(t *testing.T) {
	if err := Write(&bytes.Buffer{}, Info{}, 0, nil); err == nil {
		t.Fatal("an empty document was written")
	}
}

func TestMixedDocumentIsAValidA4PDF(t *testing.T) {
	colour, _ := JPEGPage(colourPicture(), testDPI)
	grey, _ := JPEGPage(greyPicture(), testDPI)
	lossless, _ := LosslessGreyPage(greyPicture(), testDPI)
	var document bytes.Buffer
	count, page := listed(colour, grey, lossless)
	info := Info{Created: time.Date(2026, 11, 12, 21, 44, 27, 0, time.FixedZone("CET", 3600)), Producer: "Scan Box (test)"}
	if err := Write(&document, info, count, page); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(document.String(), "%PDF-1.4") || !strings.HasSuffix(document.String(), "%%EOF\n") {
		t.Fatal("not shaped like a PDF")
	}
	if !strings.Contains(document.String(), "/MediaBox [0 0 595.44 841.68]") {
		t.Fatal("page is not A4-sized at 100 dpi")
	}
	if !strings.Contains(document.String(), `/Producer (Scan Box \(test\)) /CreationDate (D:20261112214427+01'00')`) {
		t.Fatal("the document information doesn't name the producer and the time it was made")
	}
	pdfinfo, err := exec.LookPath("pdfinfo")
	if err != nil {
		t.Skip("pdfinfo not installed; structure checked by hand only")
	}
	path := filepath.Join(t.TempDir(), "mixed.pdf")
	if err := os.WriteFile(path, document.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := exec.Command(pdfinfo, path).CombinedOutput()
	if err != nil {
		t.Fatalf("pdfinfo refused the file: %v\n%s", err, report)
	}
	if !strings.Contains(string(report), "Producer:        Scan Box (test)") || !strings.Contains(string(report), "CreationDate:") {
		t.Fatalf("pdfinfo doesn't see the document information:\n%s", report)
	}
	if !strings.Contains(string(report), "Pages:           3") || strings.Contains(strings.ToLower(string(report)), "error") {
		t.Fatalf("pdfinfo report:\n%s", report)
	}
}
