package station

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"scanbox/internal/pdf"
)

// State says where a finished document is: waiting for a name, waiting to be sent, accepted
// by the destination, stored there, or not stored.
type State string

const (
	ToReview State = "review"
	Waiting  State = "waiting"
	// Submitted is accepted by the destination, which hasn't said yet that it stored it.
	Submitted State = "submitted"
	// Sent is stored by the destination, which named the document it made.
	Sent State = "sent"
	// Failed was accepted, then not stored; LastError says why.
	Failed State = "failed"
)

const (
	openFolder        = "document"
	scansFolder       = "scans"
	pdfFile           = "document.pdf"
	infoFile          = "info.json"
	pageInfoFormat    = "page-%03d.json"
	pageDataFormat    = "page-%03d.data"
	pageThumbFormat   = "page-%03d.jpg"
	pagePreviewFormat = "page-%03d.preview.jpg"
	pageInfoPattern   = "page-*.json"
	temporarySuffix   = ".partial"
	// finishingFile names, in the open document's folder, the document its pages are
	// becoming, until they are cleared away.
	finishingFile = "finishing"
	// pageOrderFile holds, in the open document's folder, the order chosen for its pages,
	// by number. Pages it doesn't name follow, in the order they were scanned.
	pageOrderFile     = "order.json"
	filePermissions   = 0o600
	folderPermissions = 0o700
	// Folders are named the European way: 2026 / 11-2026 / 11-11-2026 / <document>.
	yearLayout       = "2006"
	monthLayout      = "01-2006"
	dayLayout        = "02-01-2006"
	idLayout         = "02-01-2006_15-04-05"
	uploadNamePrefix = "scanbox-"
	// pdfProducer names, in each PDF, the program that made it.
	pdfProducer = "Scan Box"
	colourful   = "colour"
	greyscale   = "greyscale"
	mixed       = "mixed"
)

var (
	// IDs from before date folders (20261111-214249) still work.
	idPattern = regexp.MustCompile(`^([0-9]{2}-[0-9]{2}-[0-9]{4}_[0-9]{2}-[0-9]{2}-[0-9]{2}|[0-9]{8}-[0-9]{6})(-[0-9]+)?$`)
	// legacyPlaces are the folders documents lived in before date folders.
	legacyPlaces = map[string]State{"review": ToReview, "outbox": Waiting, "sent": Sent}
	// temporaryName is a file writeAtomically had not renamed into place yet: the name it
	// was becoming, a dot and the digits os.CreateTemp added. No file Scan Box keeps ends so.
	temporaryName = regexp.MustCompile(`\.[0-9]+$`)

	ErrNotFound = errors.New("that scan isn't there any more")
)

// Document is one finished scan.
type Document struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Pages   int       `json:"pages"`
	Colour  string    `json:"colour"`
	Created time.Time `json:"created"`
	State   State     `json:"state"`
	// AttemptedAt is when an upload began whose outcome isn't settled yet; AttemptEnded,
	// when it ended without a clear answer. Both are cleared once the outcome is known.
	AttemptedAt  time.Time `json:"attempted_at,omitzero"`
	AttemptEnded time.Time `json:"attempt_ended,omitzero"`
	// Tries counts the uploads in a row that came to nothing; NextTry is when it may be
	// sent again. Both start over once an upload is accepted.
	Tries   int       `json:"tries,omitempty"`
	NextTry time.Time `json:"next_try,omitzero"`
	// SubmittedAt is when the destination accepted the upload, and Task its receipt.
	SubmittedAt time.Time `json:"submitted_at,omitzero"`
	Task        string    `json:"task,omitempty"`
	// SentAt is when the destination confirmed it stored the document, as DocumentID.
	SentAt     time.Time `json:"sent_at,omitzero"`
	DocumentID int       `json:"document_id,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
	// Size is the PDF's size in bytes.
	Size int64 `json:"size,omitempty"`
	// SentName is the title Paperless got, kept when the scan is renamed afterwards.
	SentName string `json:"sent_name,omitempty"`
	// Sending says the document is being uploaded at this moment. It is never saved.
	Sending bool `json:"-"`
}

// PaperlessTitle is the title the document has in Paperless once it is sent.
func (d Document) PaperlessTitle() string {
	if d.SentName != "" {
		return d.SentName
	}
	return d.Name
}

// settleTry forgets the uploads that came to nothing: the outcome is known.
func (d *Document) settleTry() {
	d.AttemptedAt, d.AttemptEnded, d.Tries, d.NextTry = time.Time{}, time.Time{}, 0, time.Time{}
}

// UploadName is the file name Paperless receives, so a workflow can match "scanbox-*"
// whatever name people give the document.
func (d Document) UploadName() string { return uploadNamePrefix + d.ID + ".pdf" }

// scannedPage is a page before it becomes part of a PDF.
type scannedPage struct {
	page      pdf.Page
	thumbnail []byte
	preview   []byte
}

type indexed struct {
	document Document
	folder   string
	// bytes is what the folder holds: the PDF, the page pictures and the facts.
	bytes int64
}

// disk lays the data folder out as document/ (the open document's pages) and
// scans/<year>/<month>/<day>/<document>/, and keeps every document's facts in memory
// so lists and searches don't read the disk.
type disk struct {
	root      string
	documents map[string]indexed
	// damaged lists, from the data folder, the scan folders whose facts couldn't be read.
	damaged []string
}

func newDisk(root string) *disk {
	return &disk{root: root, documents: map[string]indexed{}}
}

// prepare makes the folders, clears away what a stop in the middle of a write left,
// moves documents from the old layout into date folders in the given time zone, reads
// every document's facts, and completes a finish a stop interrupted.
func (d *disk) prepare(location *time.Location) error {
	for _, folder := range []string{openFolder, scansFolder} {
		if err := os.MkdirAll(filepath.Join(d.root, folder), folderPermissions); err != nil {
			return err
		}
	}
	if err := d.sweep(); err != nil {
		return err
	}
	if err := d.moveLegacyDocuments(location); err != nil {
		return err
	}
	if err := d.load(); err != nil {
		return err
	}
	if err := d.confirmOlderSent(); err != nil {
		return err
	}
	return d.settleFinishing()
}

// sweep removes what a stop in the middle of a write leaves behind: a document folder
// still being built, and files written but not yet renamed into place.
func (d *disk) sweep() error {
	for _, folder := range []string{scansFolder, openFolder} {
		err := filepath.WalkDir(filepath.Join(d.root, folder), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && strings.HasSuffix(entry.Name(), temporarySuffix) {
				log.Printf("removing %s, left half-written by a stop", d.relative(path))
				if err := os.RemoveAll(path); err != nil {
					return err
				}
				return fs.SkipDir
			}
			if entry.IsDir() || !temporaryName.MatchString(entry.Name()) {
				return nil
			}
			log.Printf("removing %s, left half-written by a stop", d.relative(path))
			return os.Remove(path)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// relative names a path from the data folder, as a log line shows it.
func (d *disk) relative(path string) string {
	relative, err := filepath.Rel(d.root, path)
	if err != nil {
		return path
	}
	return relative
}

func (d *disk) dateFolder(created time.Time, location *time.Location) string {
	local := created.In(location)
	return filepath.Join(d.root, scansFolder, local.Format(yearLayout), local.Format(monthLayout), local.Format(dayLayout))
}

// moveLegacyDocuments puts documents from the old review/, outbox/ and sent/ folders
// into date folders, keeping where they were as their state.
func (d *disk) moveLegacyDocuments(location *time.Location) error {
	for place, state := range legacyPlaces {
		folder := filepath.Join(d.root, place)
		entries, err := os.ReadDir(folder)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if err := d.moveLegacyDocument(filepath.Join(folder, entry.Name()), state, location); err != nil {
				return err
			}
		}
		os.Remove(folder)
	}
	return nil
}

func (d *disk) moveLegacyDocument(folder string, state State, location *time.Location) error {
	document, err := readInfo(folder)
	if err != nil {
		return nil
	}
	document.State = state
	target := filepath.Join(d.dateFolder(document.Created, location), document.ID)
	if err := os.MkdirAll(filepath.Dir(target), folderPermissions); err != nil {
		return err
	}
	if err := os.Rename(folder, target); err != nil {
		return err
	}
	return writeInfo(target, document)
}

// confirmOlderSent puts documents counted as sent before sending was confirmed back to
// submitted, so their receipt is checked and they get the destination's document ID. One
// without a receipt can't be checked, and stays sent without a document ID.
func (d *disk) confirmOlderSent() error {
	for _, document := range d.list(Sent) {
		if document.DocumentID != 0 || document.Task == "" {
			continue
		}
		document.State, document.SubmittedAt = Submitted, document.SentAt
		if err := d.save(document); err != nil {
			return err
		}
	}
	return nil
}

// load reads every document's facts once, at start. Documents from before sizes were
// recorded get theirs from the PDF.
func (d *disk) load() error {
	return filepath.WalkDir(filepath.Join(d.root, scansFolder), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || entry.Name() != infoFile || strings.Contains(path, temporarySuffix) {
			return err
		}
		folder := filepath.Dir(path)
		document, err := readInfo(folder)
		if err != nil {
			log.Printf("the scan in %s can't be read, so it isn't listed; its files are kept: %v", d.relative(folder), err)
			d.damaged = append(d.damaged, d.relative(folder))
			return nil
		}
		if document.Size == 0 {
			document.Size = fileSize(filepath.Join(folder, pdfFile))
		}
		d.documents[document.ID] = indexed{document: document, folder: folder, bytes: folderBytes(folder)}
		return nil
	})
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// folderBytes adds up the files in one document's folder.
func folderBytes(folder string) int64 {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return 0
	}
	var total int64
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && !entry.IsDir() {
			total += info.Size()
		}
	}
	return total
}

func readInfo(folder string) (Document, error) {
	content, err := os.ReadFile(filepath.Join(folder, infoFile))
	if err != nil {
		return Document{}, err
	}
	var document Document
	err = json.Unmarshal(content, &document)
	return document, err
}

func writeInfo(folder string, document Document) error {
	content, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomically(filepath.Join(folder, infoFile), content)
}

// ---- the open document ----

func (d *disk) open(name string) string { return filepath.Join(d.root, openFolder, name) }

// openPageNumbers gives the open document's pages by number, in the document's order.
func (d *disk) openPageNumbers() ([]int, error) {
	matches, err := filepath.Glob(d.open(pageInfoPattern))
	if err != nil {
		return nil, err
	}
	numbers := make([]int, 0, len(matches))
	for _, match := range matches {
		var number int
		if _, err := fmt.Sscanf(filepath.Base(match), pageInfoFormat, &number); err == nil {
			numbers = append(numbers, number)
		}
	}
	sort.Ints(numbers)
	return inOrder(numbers, d.pageOrder()), nil
}

// pageOrder is the order chosen for the open pages; none was chosen when it can't be read.
func (d *disk) pageOrder() []int {
	content, err := os.ReadFile(d.open(pageOrderFile))
	if err != nil {
		return nil
	}
	var order []int
	if err := json.Unmarshal(content, &order); err != nil {
		return nil
	}
	return order
}

// inOrder puts the page numbers in the order given, then those it doesn't name, as they
// come. Numbers in order without a page are passed over.
func inOrder(numbers, order []int) []int {
	ordered := make([]int, 0, len(numbers))
	for _, number := range order {
		if slices.Contains(numbers, number) && !slices.Contains(ordered, number) {
			ordered = append(ordered, number)
		}
	}
	for _, number := range numbers {
		if !slices.Contains(ordered, number) {
			ordered = append(ordered, number)
		}
	}
	return ordered
}

// forgetOldOrder drops an order kept from an earlier document when the open one has no
// pages yet, as after its pages were all removed: a new document reuses page numbers,
// and mustn't be shuffled by it.
func (d *disk) forgetOldOrder(numbers []int) error {
	if len(numbers) > 0 {
		return nil
	}
	err := os.Remove(d.open(pageOrderFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// moveOpenPage moves the open page with the given number one place earlier (offset -1)
// or later (offset 1), and keeps the new order.
func (d *disk) moveOpenPage(number, offset int) error {
	numbers, err := d.openPageNumbers()
	if err != nil {
		return err
	}
	from := slices.Index(numbers, number)
	if from < 0 {
		return ErrNotFound
	}
	to := from + offset
	if (offset != 1 && offset != -1) || to < 0 || to >= len(numbers) {
		return ErrCantMove
	}
	numbers[from], numbers[to] = numbers[to], numbers[from]
	content, err := json.Marshal(numbers)
	if err != nil {
		return err
	}
	return writeAtomically(d.open(pageOrderFile), content)
}

func (d *disk) addOpenPage(scanned scannedPage) (int, error) {
	numbers, err := d.openPageNumbers()
	if err != nil {
		return 0, err
	}
	if err := d.forgetOldOrder(numbers); err != nil {
		return 0, err
	}
	// A new page takes the next number after the highest, wherever that page now stands.
	number := 1
	if len(numbers) > 0 {
		number = slices.Max(numbers) + 1
	}
	info, err := json.Marshal(scanned.page)
	if err != nil {
		return 0, err
	}
	// The info file goes last: a page only counts once all its files are there.
	writes := []struct {
		name    string
		content []byte
	}{
		{fmt.Sprintf(pageDataFormat, number), scanned.page.Data},
		{fmt.Sprintf(pageThumbFormat, number), scanned.thumbnail},
		{fmt.Sprintf(pagePreviewFormat, number), scanned.preview},
		{fmt.Sprintf(pageInfoFormat, number), info},
	}
	for _, write := range writes {
		if err := writeAtomically(d.open(write.name), write.content); err != nil {
			return 0, err
		}
	}
	return len(numbers) + 1, nil
}

// readOpenPage reads one page of the open document from its files.
func (d *disk) readOpenPage(number int) (scannedPage, error) {
	var scanned scannedPage
	info, err := os.ReadFile(d.open(fmt.Sprintf(pageInfoFormat, number)))
	if err != nil {
		return scannedPage{}, err
	}
	if err := json.Unmarshal(info, &scanned.page); err != nil {
		return scannedPage{}, err
	}
	if scanned.page.Data, err = os.ReadFile(d.open(fmt.Sprintf(pageDataFormat, number))); err != nil {
		return scannedPage{}, err
	}
	scanned.thumbnail, _ = os.ReadFile(d.open(fmt.Sprintf(pageThumbFormat, number)))
	scanned.preview, _ = os.ReadFile(d.open(fmt.Sprintf(pagePreviewFormat, number)))
	return scanned, nil
}

func (d *disk) removeOpenPage(number int) error {
	for _, format := range []string{pageInfoFormat, pageDataFormat, pageThumbFormat, pagePreviewFormat} {
		if err := os.Remove(d.open(fmt.Sprintf(format, number))); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (d *disk) clearOpenDocument() error {
	numbers, err := d.openPageNumbers()
	if err != nil {
		return err
	}
	for _, number := range numbers {
		if err := d.removeOpenPage(number); err != nil {
			return err
		}
	}
	for _, name := range []string{pageOrderFile, finishingFile} {
		if err := os.Remove(d.open(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// turnOpenIntoDocument writes the open pages as document, then clears them away. The
// document's ID is noted beside the pages first, so a stop in between can't make the
// same pages a second document: the next start finds the note.
func (d *disk) turnOpenIntoDocument(document Document, count int, load func(index int) (scannedPage, error), location *time.Location) error {
	if err := writeAtomically(d.open(finishingFile), []byte(document.ID)); err != nil {
		return err
	}
	if err := d.writeDocument(document, count, load, location); err != nil {
		os.Remove(d.open(finishingFile))
		return err
	}
	return d.clearOpenDocument()
}

// settleFinishing completes a finish a stop interrupted. Pages whose document was written
// are cleared away; pages whose document wasn't stay open, to be finished again.
func (d *disk) settleFinishing() error {
	id, err := os.ReadFile(d.open(finishingFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !d.exists(string(id)) {
		return os.Remove(d.open(finishingFile))
	}
	log.Printf("the open pages were saved as scan %s before a stop; clearing them away", id)
	return d.clearOpenDocument()
}

// lastOpenPageTime is when the newest open page was scanned, wherever it now stands.
func (d *disk) lastOpenPageTime() (time.Time, bool) {
	numbers, err := d.openPageNumbers()
	if err != nil || len(numbers) == 0 {
		return time.Time{}, false
	}
	info, err := os.Stat(d.open(fmt.Sprintf(pageInfoFormat, slices.Max(numbers))))
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

func (d *disk) openPagePicture(position int, format string) (string, error) {
	numbers, err := d.openPageNumbers()
	if err != nil {
		return "", err
	}
	if position < 1 || position > len(numbers) {
		return "", ErrNotFound
	}
	return d.open(fmt.Sprintf(format, numbers[position-1])), nil
}

// ---- finished documents ----

// newID names a document after the time it was finished, the European way.
func (d *disk) newID(at time.Time, location *time.Location) string {
	base := at.In(location).Format(idLayout)
	id := base
	for attempt := 2; d.exists(id); attempt++ {
		id = fmt.Sprintf("%s-%d", base, attempt)
	}
	return id
}

func (d *disk) exists(id string) bool {
	_, found := d.documents[id]
	return found
}

// writeDocument builds the PDF in a temporary folder, from count pages that load gives
// one at a time, and renames it into its date folder, so a document is either complete
// or not there at all. Only one page is held at a time, however long the document.
func (d *disk) writeDocument(document Document, count int, load func(index int) (scannedPage, error), location *time.Location) error {
	final := filepath.Join(d.dateFolder(document.Created, location), document.ID)
	partial := final + temporarySuffix
	if err := os.MkdirAll(partial, folderPermissions); err != nil {
		return err
	}
	defer os.RemoveAll(partial)
	colourPages := 0
	page := func(index int) (pdf.Page, error) {
		scanned, err := load(index)
		if err != nil {
			return pdf.Page{}, err
		}
		if err := writePagePictures(partial, index+1, scanned); err != nil {
			return pdf.Page{}, err
		}
		if scanned.page.ColourSpace == pdf.RGB {
			colourPages++
		}
		return scanned.page, nil
	}
	info := pdf.Info{Created: document.Created.In(location), Producer: pdfProducer}
	if err := writePDF(filepath.Join(partial, pdfFile), info, count, page); err != nil {
		return err
	}
	document.Pages, document.Colour, document.Size = count, colourFor(colourPages, count), fileSize(filepath.Join(partial, pdfFile))
	if err := writeInfo(partial, document); err != nil {
		return err
	}
	if err := os.Rename(partial, final); err != nil {
		return err
	}
	syncFoldersUpTo(filepath.Dir(final), filepath.Join(d.root, scansFolder))
	d.documents[document.ID] = indexed{document: document, folder: final, bytes: folderBytes(final)}
	return nil
}

func writePagePictures(folder string, page int, scanned scannedPage) error {
	if err := writeAtomically(filepath.Join(folder, fmt.Sprintf(pageThumbFormat, page)), scanned.thumbnail); err != nil {
		return err
	}
	if len(scanned.preview) == 0 {
		return nil
	}
	return writeAtomically(filepath.Join(folder, fmt.Sprintf(pagePreviewFormat, page)), scanned.preview)
}

func writePDF(path string, info pdf.Info, count int, page func(int) (pdf.Page, error)) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, filePermissions)
	if err != nil {
		return err
	}
	if err := pdf.Write(file, info, count, page); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

// colourFor says whether a document of so many pages, so many of them in colour, is in
// colour, greyscale or mixed.
func colourFor(colourPages, pages int) string {
	switch colourPages {
	case 0:
		return greyscale
	case pages:
		return colourful
	}
	return mixed
}

func (d *disk) find(id string) (indexed, error) {
	if !idPattern.MatchString(id) {
		return indexed{}, ErrNotFound
	}
	found, exists := d.documents[id]
	if !exists {
		return indexed{}, ErrNotFound
	}
	return found, nil
}

func (d *disk) save(document Document) error {
	found, err := d.find(document.ID)
	if err != nil {
		return err
	}
	if err := writeInfo(found.folder, document); err != nil {
		return err
	}
	d.documents[document.ID] = indexed{document: document, folder: found.folder, bytes: folderBytes(found.folder)}
	return nil
}

// list gives the documents in one state, oldest first; an empty state gives all.
func (d *disk) list(state State) []Document {
	documents := make([]Document, 0, len(d.documents))
	for _, entry := range d.documents {
		if state == "" || entry.document.State == state {
			documents = append(documents, entry.document)
		}
	}
	sort.Slice(documents, func(i, j int) bool { return documents[i].Created.Before(documents[j].Created) })
	return documents
}

// remove deletes a document and any date folders it leaves empty.
func (d *disk) remove(id string) error {
	found, err := d.find(id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(found.folder); err != nil {
		return err
	}
	delete(d.documents, id)
	scans := filepath.Join(d.root, scansFolder)
	for parent := filepath.Dir(found.folder); parent != scans && strings.HasPrefix(parent, scans); parent = filepath.Dir(parent) {
		if os.Remove(parent) != nil {
			break
		}
	}
	return nil
}

func (d *disk) pdfPath(id string) (string, error) {
	found, err := d.find(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(found.folder, pdfFile), nil
}

func (d *disk) pagePicture(id string, page int, format string) (string, error) {
	found, err := d.find(id)
	if err != nil {
		return "", err
	}
	if page < 1 || page > found.document.Pages {
		return "", ErrNotFound
	}
	return filepath.Join(found.folder, fmt.Sprintf(format, page)), nil
}

// previewPath gives the large picture of a page; scans made before previews existed
// fall back to their thumbnail.
func (d *disk) previewPath(id string, page int) (string, error) {
	preview, err := d.pagePicture(id, page, pagePreviewFormat)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(preview); err == nil {
		return preview, nil
	}
	return d.pagePicture(id, page, pageThumbFormat)
}

// usedBytes adds up what the kept scans hold, from the index: nothing is read from disk.
func (d *disk) usedBytes() int64 {
	var total int64
	for _, entry := range d.documents {
		total += entry.bytes
	}
	return total
}

func writeAtomically(path string, content []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(filePermissions); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return err
	}
	syncFolder(filepath.Dir(path))
	return nil
}

// syncFolder asks for a folder's entries to reach the disk, so a rename or a new file in
// it lasts through a power cut. A file system that can't sync folders keeps its own pace.
func syncFolder(folder string) {
	handle, err := os.Open(folder)
	if err != nil {
		return
	}
	handle.Sync()
	handle.Close()
}

// syncFoldersUpTo syncs folder and each folder above it up to top, for folders that may
// have just been made inside one another, such as a new day in a new month.
func syncFoldersUpTo(folder, top string) {
	for strings.HasPrefix(folder, top) {
		syncFolder(folder)
		if folder == top {
			return
		}
		folder = filepath.Dir(folder)
	}
}
