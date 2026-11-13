package station

import (
	"image"
	"image/color"
	"testing"
)

func TestHalfTurnPutsTheTopLeftAtTheBottomRight(t *testing.T) {
	t.Parallel()
	grey := image.NewGray(image.Rect(0, 0, 3, 2))
	grey.SetGray(0, 0, color.Gray{Y: 200})
	turned := halfTurn(grey).(*image.Gray)
	if turned.GrayAt(2, 1).Y != 200 || turned.GrayAt(0, 0).Y != 0 {
		t.Fatalf("grey page not turned: %v", turned.Pix)
	}

	colour := image.NewRGBA(image.Rect(0, 0, 3, 3))
	colour.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
	colour.SetRGBA(1, 1, color.RGBA{G: 255, A: 255})
	turnedColour := halfTurn(colour).(*image.RGBA)
	if turnedColour.RGBAAt(2, 2).R != 255 || turnedColour.RGBAAt(1, 1).G != 255 || turnedColour.RGBAAt(0, 0).R != 0 {
		t.Fatalf("colour page not turned: %v", turnedColour.Pix)
	}

	paletted := image.NewPaletted(image.Rect(0, 0, 2, 1), color.Palette{color.Black, color.White})
	paletted.SetColorIndex(0, 0, 1)
	other := halfTurn(paletted)
	if r, _, _, _ := other.At(1, 0).RGBA(); r == 0 {
		t.Fatal("other kinds of picture are not turned")
	}
}
