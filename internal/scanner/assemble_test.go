package scanner

import (
	"image"
	"math/rand/v2"
	"runtime"
	"testing"
)

// scanBytes is what SANE would send for a page: lines of width pixels, lineBytes long
// (padding after the pixels), each byte telling its place.
func scanBytes(width, lines, lineBytes, samples int) []byte {
	data := make([]byte, lines*lineBytes)
	for y := 0; y < lines; y++ {
		for x := 0; x < width*samples; x++ {
			data[y*lineBytes+x] = byte(x*7 + y*13)
		}
	}
	return data
}

// feed hands data over in pieces of random sizes, as sane_read does.
func feed(scan *picture, data []byte, random *rand.Rand) {
	for len(data) > 0 {
		size := min(len(data), 1+random.IntN(5000))
		scan.Write(data[:size])
		data = data[size:]
	}
}

func TestAScanIsAssembledLineByLine(t *testing.T) {
	random := rand.New(rand.NewPCG(1, 2))
	const width, lines = 301, 97
	cases := map[string]struct {
		colour      bool
		lineBytes   int
		knownLines  int
		samples     int
		wantPadding bool
	}{
		"colour":                         {true, width * bytesPerRGBPixel, lines, bytesPerRGBPixel, false},
		"colour, padded lines":           {true, width*bytesPerRGBPixel + 5, lines, bytesPerRGBPixel, true},
		"grey":                           {false, width, lines, 1, false},
		"grey, length unknown, it grows": {false, width + 3, unknownLines, 1, true},
	}
	for name, example := range cases {
		data := scanBytes(width, lines, example.lineBytes, example.samples)
		scan := newPicture(example.colour, width, example.lineBytes, example.knownLines)
		if example.knownLines == unknownLines {
			scan = newPicture(example.colour, width, example.lineBytes, unknownLines)
			scan.grey = image.NewGray(image.Rect(0, 0, width, 10))
		}
		feed(scan, data, random)
		got := scan.Image()
		if got.Bounds() != image.Rect(0, 0, width, lines) {
			t.Errorf("%s: bounds %v", name, got.Bounds())
			continue
		}
		for _, point := range [][2]int{{0, 0}, {width - 1, 0}, {150, 50}, {width - 1, lines - 1}} {
			x, y := point[0], point[1]
			offset := y*example.lineBytes + x*example.samples
			if example.colour {
				pixel := got.(*image.RGBA).RGBAAt(x, y)
				if pixel.R != data[offset] || pixel.G != data[offset+1] || pixel.B != data[offset+2] || pixel.A != opaque {
					t.Errorf("%s: pixel %v is %v", name, point, pixel)
				}
				continue
			}
			if level := got.(*image.Gray).GrayAt(x, y).Y; level != data[offset] {
				t.Errorf("%s: pixel %v is %d, want %d", name, point, level, data[offset])
			}
		}
	}
}

func TestAPageIsHeldOnce(t *testing.T) {
	// A colour A4 page at 300 dpi, as SANE sends it, in 64 KiB pieces made one at a time.
	const width, lines = 2480, 3508
	lineBytes := width * bytesPerRGBPixel
	piece := make([]byte, 64*1024)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	scan := newPicture(true, width, lineBytes, lines)
	for remaining := lines * lineBytes; remaining > 0; remaining -= len(piece) {
		scan.Write(piece[:min(len(piece), remaining)])
	}
	runtime.ReadMemStats(&after)
	picture := width * lines * bytesPerRGBA
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(picture)*105/100 {
		t.Fatalf("a page of %d MB took %d MB to assemble; it should be held once", picture>>20, allocated>>20)
	}
	if scan.Image().Bounds().Dy() != lines {
		t.Fatal("rows went missing")
	}
}
