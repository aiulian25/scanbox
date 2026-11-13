package station

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"scanbox/internal/settings"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), folderPermissions); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), filePermissions); err != nil {
		t.Fatal(err)
	}
}

func facts(t *testing.T, id string) string {
	t.Helper()
	content, err := json.Marshal(Document{ID: id, Name: "Gas bill", Pages: 1, Created: time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC), State: Sent})
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func gone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, os.ErrNotExist)
}

// A stop in the middle of a write leaves a document folder still being built and files not
// yet renamed into place; the next start clears them away and keeps everything else.
func TestTheStartClearsAwayHalfWrittenFiles(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	day := filepath.Join(root, scansFolder, "2026", "11-2026", "12-11-2026")
	kept := filepath.Join(day, "12-11-2026_10-00-00")
	writeFile(t, filepath.Join(kept, infoFile), facts(t, "12-11-2026_10-00-00"))
	writeFile(t, filepath.Join(kept, infoFile+".3129812391"), "{")
	partial := filepath.Join(day, "12-11-2026_11-00-00"+temporarySuffix)
	writeFile(t, filepath.Join(partial, infoFile), facts(t, "12-11-2026_11-00-00"))
	writeFile(t, filepath.Join(root, openFolder, "page-001.json.42"), "{")
	writeFile(t, filepath.Join(root, openFolder, "page-001.json"), "{}")
	disk := newDisk(root)
	if err := disk.prepare(time.UTC); err != nil {
		t.Fatal(err)
	}
	for _, leftover := range []string{partial, filepath.Join(kept, infoFile+".3129812391"), filepath.Join(root, openFolder, "page-001.json.42")} {
		if !gone(leftover) {
			t.Errorf("%s was left behind", leftover)
		}
	}
	for _, file := range []string{filepath.Join(kept, infoFile), filepath.Join(root, openFolder, "page-001.json")} {
		if gone(file) {
			t.Errorf("%s was removed", file)
		}
	}
	if ids := slices.Collect(maps.Keys(disk.documents)); !slices.Equal(ids, []string{"12-11-2026_10-00-00"}) {
		t.Fatalf("listed %v, want only the finished scan", ids)
	}
}

// A scan whose facts can't be read is named, not silently left out, and its files stay.
func TestAScanThatCantBeReadIsNamed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	folder := filepath.Join(root, scansFolder, "2026", "11-2026", "12-11-2026", "12-11-2026_12-00-00")
	writeFile(t, filepath.Join(folder, infoFile), `{"id": "12-11-2026_12-00-00", "name": `)
	writeFile(t, filepath.Join(folder, pdfFile), "%PDF-1.4")
	disk := newDisk(root)
	if err := disk.prepare(time.UTC); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(scansFolder, "2026", "11-2026", "12-11-2026", "12-11-2026_12-00-00")
	if !slices.Equal(disk.damaged, []string{want}) || len(disk.documents) != 0 {
		t.Fatalf("damaged %v, listed %d; want the folder named and nothing listed", disk.damaged, len(disk.documents))
	}
	if gone(filepath.Join(folder, pdfFile)) {
		t.Fatal("the unreadable scan's PDF was removed")
	}
}

// openTwoPages leaves the rig stopped with a document of two pages open.
func openTwoPages(t *testing.T, test *rig) {
	t.Helper()
	test.perform(t, settings.AddGreyPage)
	test.waitFor(t, "an open page", func(status Status) bool { return status.OpenPages == 1 })
	test.perform(t, settings.AddGreyPage)
	test.waitFor(t, "two open pages", func(status Status) bool { return status.OpenPages == 2 })
	test.stop()
}

// interruptFinish does what finishing does up to a stop: the note beside the open pages,
// and, when written is true, the document itself; the pages aren't cleared.
func interruptFinish(t *testing.T, test *rig, written bool) {
	t.Helper()
	station := test.station
	station.files.Lock()
	defer station.files.Unlock()
	document := Document{ID: "12-11-2026_10-00-00", Name: "Interrupted", Created: test.now(), State: ToReview}
	if err := writeAtomically(station.disk.open(finishingFile), []byte(document.ID)); err != nil {
		t.Fatal(err)
	}
	if !written {
		return
	}
	numbers, err := station.disk.openPageNumbers()
	if err != nil {
		t.Fatal(err)
	}
	load := func(index int) (scannedPage, error) { return station.disk.readOpenPage(numbers[index]) }
	if err := station.disk.writeDocument(document, len(numbers), load, test.settings.Get().Clock().Location); err != nil {
		t.Fatal(err)
	}
}

func TestAStopAfterFinishingDoesntMakeASecondCopy(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	openTwoPages(t, test)
	interruptFinish(t, test, true)
	status := test.restart(t, test.destination, time.Hour).Status()
	if status.OpenPages != 0 || len(status.Review) != 1 || status.Review[0].Pages != 2 {
		t.Fatalf("after the restart: %d open pages, %d scans; want the one scan of two pages and no open pages", status.OpenPages, len(status.Review))
	}
	if !gone(filepath.Join(test.directory, openFolder, finishingFile)) {
		t.Fatal("the note was left beside the cleared pages")
	}
}

func TestAStopBeforeTheDocumentWasWrittenKeepsThePagesOpen(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	openTwoPages(t, test)
	interruptFinish(t, test, false)
	status := test.restart(t, test.destination, time.Hour).Status()
	if status.OpenPages != 2 || len(status.Review) != 0 {
		t.Fatalf("after the restart: %d open pages, %d scans; want the two pages still open", status.OpenPages, len(status.Review))
	}
	if !gone(filepath.Join(test.directory, openFolder, finishingFile)) {
		t.Fatal("the note of a finish that never wrote its document was kept")
	}
}

// The open document's pages can be put in another order before it is finished; the PDF
// follows it, a new page comes last, and "remove the last page" takes the one shown last.
func TestOpenPagesCanBeReordered(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.AddColourPage)
	test.waitFor(t, "a colour page", func(status Status) bool { return status.OpenPages == 1 })
	test.perform(t, settings.AddGreyPage)
	numbers := test.waitFor(t, "a grey page after it", func(status Status) bool { return status.OpenPages == 2 }).OpenPageNumbers
	colour, grey := numbers[0], numbers[1]
	if err := test.station.MoveOpenPage(colour, -1); !errors.Is(err, ErrCantMove) {
		t.Fatalf("moving the first page earlier: %v, want ErrCantMove", err)
	}
	if err := test.station.MoveOpenPage(grey, 1); !errors.Is(err, ErrCantMove) {
		t.Fatalf("moving the last page later: %v, want ErrCantMove", err)
	}
	if err := test.station.MoveOpenPage(grey+colour+1, 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("moving a page that isn't there: %v, want ErrNotFound", err)
	}
	if err := test.station.MoveOpenPage(grey, -1); err != nil {
		t.Fatal(err)
	}
	if order := test.station.Status().OpenPageNumbers; !slices.Equal(order, []int{grey, colour}) {
		t.Fatalf("after moving the grey page earlier the order is %v", order)
	}
	test.perform(t, settings.AddColourPage)
	order := test.waitFor(t, "a third page", func(status Status) bool { return status.OpenPages == 3 }).OpenPageNumbers
	if order[0] != grey || order[1] != colour || order[2] <= max(grey, colour) {
		t.Fatalf("a new page should come last, with a new number: %v", order)
	}
	newest, _ := test.station.disk.lastOpenPageTime()
	third, err := os.Stat(test.station.disk.open(fmt.Sprintf(pageInfoFormat, order[2])))
	if err != nil || !newest.Equal(third.ModTime()) {
		t.Fatalf("the idle finish should count from the newest page, not from the one shown last")
	}
	if err := test.station.RemoveLastPage(); err != nil {
		t.Fatal(err)
	}
	test.waitFor(t, "the last page removed", func(status Status) bool { return slices.Equal(status.OpenPageNumbers, []int{grey, colour}) })
	test.perform(t, settings.FinishDocument)
	document := test.waitFor(t, "the finished scan", func(status Status) bool { return len(status.Review) == 1 }).Review[0]
	path, err := test.station.PDFPath(document.ID)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	greyAt, colourAt := strings.Index(string(content), "/ColorSpace /DeviceGray"), strings.Index(string(content), "/ColorSpace /DeviceRGB")
	if document.Pages != 2 || greyAt < 0 || colourAt < 0 || greyAt > colourAt {
		t.Fatalf("the PDF should hold the grey page first, then the colour one (%d pages; grey at %d, colour at %d)", document.Pages, greyAt, colourAt)
	}
	if !gone(test.station.disk.open(pageOrderFile)) {
		t.Fatal("the chosen order was left beside the cleared pages")
	}
}

// Removing every page and starting again gives a document in scan order: the new pages
// reuse numbers the old order named. A page added to a reordered document keeps the order.
func TestANewDocumentDoesntInheritAnOldOrder(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	addPage := func(count int) {
		test.perform(t, settings.AddGreyPage)
		test.waitFor(t, "a page", func(status Status) bool { return status.OpenPages == count })
	}
	addPage(1)
	addPage(2)
	numbers := test.station.Status().OpenPageNumbers
	if err := test.station.MoveOpenPage(numbers[1], -1); err != nil {
		t.Fatal(err)
	}
	addPage(3)
	if order := test.station.Status().OpenPageNumbers; order[0] != numbers[1] || order[1] != numbers[0] {
		t.Fatalf("adding a page undid the chosen order: %v", order)
	}
	for count := 2; count >= 0; count-- {
		if err := test.station.RemoveLastPage(); err != nil {
			t.Fatal(err)
		}
		test.waitFor(t, "a page removed", func(status Status) bool { return status.OpenPages == count })
	}
	addPage(1)
	addPage(2)
	if order := test.station.Status().OpenPageNumbers; !slices.IsSorted(order) {
		t.Fatalf("a new document took the order of the one before: %v", order)
	}
}

func TestTheChosenOrderAndThenTheRest(t *testing.T) {
	t.Parallel()
	cases := []struct{ numbers, order, want []int }{
		{[]int{1, 2, 3}, nil, []int{1, 2, 3}},
		{[]int{1, 2, 3}, []int{3, 1, 2}, []int{3, 1, 2}},
		{[]int{1, 2, 3, 4}, []int{2, 1}, []int{2, 1, 3, 4}},
		{[]int{1, 3}, []int{3, 2, 1}, []int{3, 1}},
		{[]int{1, 2}, []int{2, 2, 1}, []int{2, 1}},
	}
	for _, example := range cases {
		if got := inOrder(example.numbers, example.order); !slices.Equal(got, example.want) {
			t.Errorf("inOrder(%v, %v) = %v, want %v", example.numbers, example.order, got, example.want)
		}
	}
}

func TestFinishingLeavesNoNote(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.AddGreyPage)
	test.waitFor(t, "an open page", func(status Status) bool { return status.OpenPages == 1 })
	test.perform(t, settings.FinishDocument)
	test.waitFor(t, "the finished scan", func(status Status) bool { return len(status.Review) == 1 && status.OpenPages == 0 })
	if !gone(filepath.Join(test.directory, openFolder, finishingFile)) {
		t.Fatal("finishing left its note behind")
	}
}
