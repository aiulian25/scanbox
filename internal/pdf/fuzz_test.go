package pdf

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"testing"
	"time"
)

var (
	startCrossReference = regexp.MustCompile(`startxref\n([0-9]+)\n%%EOF\n$`)
	crossReferenceEntry = regexp.MustCompile(`^([0-9]{10}) 00000 n \n`)
	scanResolutions     = []int{150, 300, 600}
)

// checkCrossReference checks what a PDF reader looks at first: startxref points at the
// cross-reference table, the table lists every object, and each entry points at the
// object it names, whatever the page data holds.
func checkCrossReference(t *testing.T, document []byte, objects int) {
	t.Helper()
	found := startCrossReference.FindSubmatch(document)
	if found == nil {
		t.Fatal("the PDF doesn't end with startxref and its end-of-file marker")
	}
	start, _ := strconv.Atoi(string(found[1]))
	table := fmt.Sprintf("xref\n0 %d\n0000000000 65535 f \n", objects+1)
	if start > len(document) || !bytes.HasPrefix(document[start:], []byte(table)) {
		t.Fatalf("startxref %d doesn't point at a table of %d objects", start, objects)
	}
	entries := document[start+len(table):]
	for number := 1; number <= objects; number++ {
		entry := crossReferenceEntry.FindSubmatch(entries)
		if entry == nil {
			t.Fatalf("the table has no entry for object %d", number)
		}
		offset, _ := strconv.Atoi(string(entry[1]))
		if offset > len(document) || !bytes.HasPrefix(document[offset:], fmt.Appendf(nil, "%d 0 obj\n", number)) {
			t.Fatalf("object %d isn't at offset %d", number, offset)
		}
		entries = entries[len(entry[0]):]
	}
}

func FuzzWriteKeepsItsCrossReference(f *testing.F) {
	f.Add(uint8(1), uint16(1240), uint16(1754), []byte("\xff\xd8 endstream endobj xref 3 0 obj trailer"))
	f.Add(uint8(3), uint16(0), uint16(0), []byte{})
	f.Add(uint8(7), uint16(65535), uint16(1), []byte("%%EOF\nstartxref\n0\n%%EOF\n"))
	f.Fuzz(func(t *testing.T, count uint8, width, height uint16, data []byte) {
		pages := int(count%8) + 1
		var document bytes.Buffer
		info := Info{Created: time.Unix(int64(width)*int64(height), 0), Producer: string(data[:min(len(data), 16)])}
		err := Write(&document, info, pages, func(index int) (Page, error) {
			resolution := scanResolutions[index%len(scanResolutions)]
			return Page{Width: int(width) + 1, Height: int(height) + 1, DPI: resolution, ColourSpace: Grey, Encoding: JPEG, Data: data}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		checkCrossReference(t, document.Bytes(), pagesObject+objectsPerPage*pages+1)
	})
}
