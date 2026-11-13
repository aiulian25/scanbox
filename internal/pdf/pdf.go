// Package pdf turns scanned pages into a PDF. Each page's image goes in as it was
// stored (JPEG, or lossless greyscale), without being decoded or re-encoded.
package pdf

import (
	"bytes"
	"compress/zlib"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"strings"
	"time"
)

const (
	JPEGQuality      = 85
	pointsPerInch    = 72
	bitsPerComponent = 8
	header           = "%PDF-1.4\n%\xe2\xe3\xcf\xd3\n"
	catalogObject    = 1
	pagesObject      = 2
	objectsPerPage   = 3
	// A PDF date is D:20261112214427+01'00': the time, then the offset from UTC with quotes
	// where Go writes a colon and nothing.
	dateLayout   = "D:20060102150405"
	offsetLayout = "-07:00"
)

// literalText escapes the three characters a PDF literal string can't hold as they are.
var literalText = strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`)

// Info is what a PDF says about itself: when it was made, and by what.
type Info struct {
	Created  time.Time
	Producer string
}

type Encoding string

const (
	JPEG  Encoding = "DCTDecode"
	Flate Encoding = "FlateDecode"
)

type ColourSpace string

const (
	Grey ColourSpace = "DeviceGray"
	RGB  ColourSpace = "DeviceRGB"
)

// Page is one scanned page with its image already encoded for the PDF.
type Page struct {
	Width       int         `json:"width"`
	Height      int         `json:"height"`
	DPI         int         `json:"dpi"`
	ColourSpace ColourSpace `json:"colour_space"`
	Encoding    Encoding    `json:"encoding"`
	Data        []byte      `json:"-"`
}

// JPEGPage encodes a colour or greyscale image as JPEG.
func JPEGPage(picture image.Image, dpi int) (Page, error) {
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, picture, &jpeg.Options{Quality: JPEGQuality}); err != nil {
		return Page{}, err
	}
	return pageFor(picture, dpi, colourSpaceOf(picture), JPEG, encoded.Bytes()), nil
}

// LosslessGreyPage keeps every grey level exactly, compressed with zlib.
func LosslessGreyPage(picture *image.Gray, dpi int) (Page, error) {
	var encoded bytes.Buffer
	compressor := zlib.NewWriter(&encoded)
	bounds := picture.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		start := picture.PixOffset(bounds.Min.X, y)
		if _, err := compressor.Write(picture.Pix[start : start+bounds.Dx()]); err != nil {
			return Page{}, err
		}
	}
	if err := compressor.Close(); err != nil {
		return Page{}, err
	}
	return pageFor(picture, dpi, Grey, Flate, encoded.Bytes()), nil
}

func pageFor(picture image.Image, dpi int, colourSpace ColourSpace, encoding Encoding, data []byte) Page {
	bounds := picture.Bounds()
	return Page{Width: bounds.Dx(), Height: bounds.Dy(), DPI: dpi, ColourSpace: colourSpace, Encoding: encoding, Data: data}
}

func colourSpaceOf(picture image.Image) ColourSpace {
	if picture.ColorModel() == color.GrayModel {
		return Grey
	}
	return RGB
}

// countingWriter remembers how many bytes went out, for the cross-reference table.
type countingWriter struct {
	writer io.Writer
	count  int64
	err    error
}

func (w *countingWriter) printf(format string, arguments ...any) {
	if w.err != nil {
		return
	}
	written, err := fmt.Fprintf(w.writer, format, arguments...)
	w.count += int64(written)
	w.err = err
}

func (w *countingWriter) write(data []byte) {
	if w.err != nil {
		return
	}
	written, err := w.writer.Write(data)
	w.count += int64(written)
	w.err = err
}

// Write puts count pages into one PDF, each page sized to its scan, with info as its
// document information. It asks for one page at a time and is done with it before the
// next, so a long document is never held whole.
func Write(destination io.Writer, info Info, count int, page func(index int) (Page, error)) error {
	if count == 0 {
		return fmt.Errorf("a PDF needs at least one page")
	}
	output := &countingWriter{writer: destination}
	infoObject := pagesObject + objectsPerPage*count + 1
	objectCount := infoObject
	offsets := make([]int64, objectCount+1)
	begin := func(number int) {
		offsets[number] = output.count
		output.printf("%d 0 obj\n", number)
	}

	output.printf("%s", header)
	begin(catalogObject)
	output.printf("<< /Type /Catalog /Pages %d 0 R >>\nendobj\n", pagesObject)
	begin(pagesObject)
	output.printf("<< /Type /Pages /Count %d /Kids [", count)
	for index := range count {
		output.printf(" %d 0 R", pageObject(index))
	}
	output.printf(" ] >>\nendobj\n")

	for index := range count {
		page, err := page(index)
		if err != nil {
			return err
		}
		width, height := pointSize(page.Width, page.DPI), pointSize(page.Height, page.DPI)
		begin(pageObject(index))
		output.printf("<< /Type /Page /Parent %d 0 R /MediaBox [0 0 %.2f %.2f] /Resources << /XObject << /Im0 %d 0 R >> >> /Contents %d 0 R >>\nendobj\n",
			pagesObject, width, height, pageObject(index)+2, pageObject(index)+1)
		drawing := fmt.Sprintf("q %.2f 0 0 %.2f 0 0 cm /Im0 Do Q", width, height)
		begin(pageObject(index) + 1)
		output.printf("<< /Length %d >>\nstream\n%s\nendstream\nendobj\n", len(drawing), drawing)
		begin(pageObject(index) + 2)
		output.printf("<< /Type /XObject /Subtype /Image /Width %d /Height %d /ColorSpace /%s /BitsPerComponent %d /Filter /%s /Length %d >>\nstream\n",
			page.Width, page.Height, page.ColourSpace, bitsPerComponent, page.Encoding, len(page.Data))
		output.write(page.Data)
		output.printf("\nendstream\nendobj\n")
	}
	begin(infoObject)
	output.printf("<< /Producer (%s) /CreationDate (%s) >>\nendobj\n", literalText.Replace(info.Producer), pdfDate(info.Created))

	crossReference := output.count
	output.printf("xref\n0 %d\n0000000000 65535 f \n", objectCount+1)
	for number := 1; number <= objectCount; number++ {
		output.printf("%010d 00000 n \n", offsets[number])
	}
	output.printf("trailer\n<< /Size %d /Root %d 0 R /Info %d 0 R >>\nstartxref\n%d\n%%%%EOF\n", objectCount+1, catalogObject, infoObject, crossReference)
	return output.err
}

// pdfDate writes a moment, in its own zone, as PDF writes dates: D:20261112214427+01'00'.
func pdfDate(moment time.Time) string {
	return moment.Format(dateLayout) + strings.Replace(moment.Format(offsetLayout), ":", "'", 1) + "'"
}

func pageObject(index int) int {
	return pagesObject + 1 + objectsPerPage*index
}

func pointSize(pixels, dpi int) float64 {
	return float64(pixels) * pointsPerInch / float64(dpi)
}
