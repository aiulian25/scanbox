package station

import (
	"bytes"
	"image"
	"image/draw"
	"image/jpeg"

	"scanbox/internal/pdf"
	"scanbox/internal/scanner"
	"scanbox/internal/settings"
)

const (
	thumbnailWidth    = 200
	thumbnailQuality  = 70
	previewWidth      = 1240
	previewQuality    = 80
	samplesPerPixel   = 4
	eightBits         = 8
	opaque            = 255
	millimetresA4W    = 210
	millimetresA4H    = 297
	letterWidthMM     = 215.9
	letterHeightMM    = 279.4
	bytesPerRGBAPixel = 4
)

var paperAreas = map[settings.PaperSize]scanner.Area{
	settings.PaperA4:     {WidthMM: millimetresA4W, HeightMM: millimetresA4H},
	settings.PaperLetter: {WidthMM: letterWidthMM, HeightMM: letterHeightMM},
	settings.PaperGlass:  {},
}

func requestFor(current settings.Settings, mode scanner.Mode) scanner.Request {
	return scanner.Request{Mode: mode, ResolutionDPI: current.ResolutionDPI, Area: paperAreas[current.PaperSize]}
}

// encodePage stores colour as JPEG, and greyscale as JPEG or lossless as the settings say.
func encodePage(picture image.Image, mode scanner.Mode, current settings.Settings) (scannedPage, error) {
	if current.TurnPages {
		picture = halfTurn(picture)
	}
	thumbnail, err := shrink(picture, thumbnailWidth, thumbnailQuality)
	if err != nil {
		return scannedPage{}, err
	}
	preview, err := shrink(picture, previewWidth, previewQuality)
	if err != nil {
		return scannedPage{}, err
	}
	lossless := mode == scanner.Grey && current.GreyStorage == settings.GreyLossless
	var page pdf.Page
	if lossless {
		page, err = pdf.LosslessGreyPage(asGrey(picture), current.ResolutionDPI)
	} else {
		page, err = pdf.JPEGPage(picture, current.ResolutionDPI)
	}
	return scannedPage{page: page, thumbnail: thumbnail, preview: preview}, err
}

// halfTurn turns a page upside down. Scans arrive as *image.Gray or *image.RGBA,
// which are turned in place; anything else is copied.
func halfTurn(picture image.Image) image.Image {
	switch page := picture.(type) {
	case *image.Gray:
		reverseRows(page.Pix, page.Stride, page.Rect.Dy(), page.Rect.Dx(), 1)
		return page
	case *image.RGBA:
		reverseRows(page.Pix, page.Stride, page.Rect.Dy(), page.Rect.Dx(), bytesPerRGBAPixel)
		return page
	}
	bounds := picture.Bounds()
	turned := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			turned.Set(bounds.Dx()-1-x, bounds.Dy()-1-y, picture.At(bounds.Min.X+x, bounds.Min.Y+y))
		}
	}
	return turned
}

// reverseRows swaps pixel (x, y) with (width-1-x, height-1-y), in place.
func reverseRows(pixels []byte, stride, height, width, pixelBytes int) {
	swapPixels := func(first, second int) {
		for offset := 0; offset < pixelBytes; offset++ {
			pixels[first+offset], pixels[second+offset] = pixels[second+offset], pixels[first+offset]
		}
	}
	total := height * width
	for index := 0; index < total/2; index++ {
		mirrored := total - 1 - index
		swapPixels(index/width*stride+index%width*pixelBytes, mirrored/width*stride+mirrored%width*pixelBytes)
	}
}

func asGrey(picture image.Image) *image.Gray {
	if grey, isGrey := picture.(*image.Gray); isGrey {
		return grey
	}
	grey := image.NewGray(picture.Bounds())
	draw.Draw(grey, grey.Bounds(), picture, picture.Bounds().Min, draw.Src)
	return grey
}

// shrink scales a page down to the given width as a JPEG, averaging four samples per
// pixel so text stays readable. Scans arrive as *image.Gray or *image.RGBA, read directly.
func shrink(picture image.Image, width, quality int) ([]byte, error) {
	bounds := picture.Bounds()
	width = min(width, bounds.Dx())
	height := bounds.Dy() * width / bounds.Dx()
	stepX, stepY := float64(bounds.Dx())/float64(width), float64(bounds.Dy())/float64(height)
	sample := sampler(picture)
	small := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			left, top := bounds.Min.X+int(float64(x)*stepX), bounds.Min.Y+int(float64(y)*stepY)
			right, bottom := left+int(stepX/2), top+int(stepY/2)
			var red, green, blue int
			for _, point := range [][2]int{{left, top}, {right, top}, {left, bottom}, {right, bottom}} {
				r, g, b := sample(point[0], point[1])
				red, green, blue = red+r, green+g, blue+b
			}
			offset := small.PixOffset(x, y)
			small.Pix[offset], small.Pix[offset+1], small.Pix[offset+2], small.Pix[offset+3] = uint8(red/samplesPerPixel), uint8(green/samplesPerPixel), uint8(blue/samplesPerPixel), opaque
		}
	}
	var encoded bytes.Buffer
	err := jpeg.Encode(&encoded, small, &jpeg.Options{Quality: quality})
	return encoded.Bytes(), err
}

func sampler(picture image.Image) func(x, y int) (int, int, int) {
	switch page := picture.(type) {
	case *image.Gray:
		return func(x, y int) (int, int, int) {
			level := int(page.Pix[page.PixOffset(x, y)])
			return level, level, level
		}
	case *image.RGBA:
		return func(x, y int) (int, int, int) {
			offset := page.PixOffset(x, y)
			return int(page.Pix[offset]), int(page.Pix[offset+1]), int(page.Pix[offset+2])
		}
	}
	return func(x, y int) (int, int, int) {
		r, g, b, _ := picture.At(x, y).RGBA()
		return int(r >> eightBits), int(g >> eightBits), int(b >> eightBits)
	}
}
