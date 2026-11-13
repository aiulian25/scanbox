package scanner

import "image"

const (
	bytesPerRGBPixel = 3
	bytesPerRGBA     = 4
	opaque           = 255
	// unknownLines is what SANE says when it doesn't know the scan's length ahead.
	unknownLines = -1
	// startingRows is the first guess at a scan's length when SANE doesn't know it.
	startingRows = 1024
)

// picture assembles a scan from the bytes SANE hands over, in pieces of any size, straight
// into the final picture, one line at a time. So a page is held once: there is no raw copy
// to convert, and nothing grows while a scan whose length SANE knows comes in.
type picture struct {
	grey   *image.Gray
	colour *image.RGBA
	// width is in pixels; lineBytes is how many bytes SANE sends for one line.
	width     int
	lineBytes int
	line      []byte
	filled    int
	rows      int
}

// newPicture makes room for lines lines of width pixels, as SANE's parameters give them,
// in colour or grey; a length SANE doesn't know starts with a guess, and grows.
func newPicture(colour bool, width, lineBytes, lines int) *picture {
	if lines == unknownLines || lines <= 0 {
		lines = startingRows
	}
	built := &picture{width: width, lineBytes: lineBytes, line: make([]byte, lineBytes)}
	bounds := image.Rect(0, 0, width, lines)
	if colour {
		built.colour = image.NewRGBA(bounds)
		return built
	}
	built.grey = image.NewGray(bounds)
	return built
}

// Write takes the next bytes of the scan, whatever their length.
func (p *picture) Write(data []byte) (int, error) {
	taken := len(data)
	for len(data) > 0 {
		copied := copy(p.line[p.filled:], data)
		p.filled += copied
		data = data[copied:]
		if p.filled < p.lineBytes {
			continue
		}
		p.addLine()
		p.filled = 0
	}
	return taken, nil
}

// addLine puts the line just completed into the next row, making room if the scan is
// longer than expected.
func (p *picture) addLine() {
	if p.rows == p.capacity() {
		p.grow()
	}
	if p.grey != nil {
		copy(p.grey.Pix[p.rows*p.grey.Stride:], p.line[:p.width])
		p.rows++
		return
	}
	row := p.colour.Pix[p.rows*p.colour.Stride:]
	for x := 0; x < p.width; x++ {
		copy(row[x*bytesPerRGBA:x*bytesPerRGBA+bytesPerRGBPixel], p.line[x*bytesPerRGBPixel:x*bytesPerRGBPixel+bytesPerRGBPixel])
		row[x*bytesPerRGBA+bytesPerRGBPixel] = opaque
	}
	p.rows++
}

func (p *picture) capacity() int {
	if p.grey != nil {
		return p.grey.Rect.Dy()
	}
	return p.colour.Rect.Dy()
}

// grow doubles the rows there is room for.
func (p *picture) grow() {
	bounds := image.Rect(0, 0, p.width, 2*p.capacity())
	if p.grey != nil {
		larger := image.NewGray(bounds)
		copy(larger.Pix, p.grey.Pix)
		p.grey = larger
		return
	}
	larger := image.NewRGBA(bounds)
	copy(larger.Pix, p.colour.Pix)
	p.colour = larger
}

// Image is the scan, as many rows as came in, sharing the picture's memory.
func (p *picture) Image() image.Image {
	bounds := image.Rect(0, 0, p.width, p.rows)
	if p.grey != nil {
		return &image.Gray{Pix: p.grey.Pix[:p.rows*p.grey.Stride], Stride: p.grey.Stride, Rect: bounds}
	}
	return &image.RGBA{Pix: p.colour.Pix[:p.rows*p.colour.Stride], Stride: p.colour.Stride, Rect: bounds}
}
