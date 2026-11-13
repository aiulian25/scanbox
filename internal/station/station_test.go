package station

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"scanbox/internal/scanner"
	"scanbox/internal/settings"
)

const (
	testTick    = 5 * time.Millisecond
	waitLimit   = 30 * time.Second
	sampleTitle = "Gas bill November"
)

type upload struct {
	title, fileName string
	scanned         time.Time
	content         []byte
}

// storedDocumentID is the document a fake destination says it stored.
const storedDocumentID = 1234

type fakeDestination struct {
	mutex   sync.Mutex
	failing bool
	// losing keeps each upload but loses the answer; dropping loses the upload itself.
	// Either way the upload ends unclear.
	losing   bool
	dropping bool
	// rejecting is a file name the destination refuses; denying refuses every upload.
	rejecting   string
	denying     bool
	attempts    int
	attemptsFor map[string]int
	uploads     []upload
	// received holds each kept upload's receipt by file name, for Find.
	received map[string]string
	// confirming answers Confirm; until it is set, every upload is stored at once.
	confirming func(receipt string) (Delivery, error)
}

func (f *fakeDestination) Find(_ context.Context, fileName string) (string, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.received[fileName], nil
}

func (f *fakeDestination) change(change func(*fakeDestination)) {
	f.mutex.Lock()
	change(f)
	f.mutex.Unlock()
}

func (f *fakeDestination) tries() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.attempts
}

func (f *fakeDestination) triesOf(fileName string) int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.attemptsFor[fileName]
}

// waitingPair leaves two scans waiting while no station runs, so the next station's first
// upload round meets both. It gives the rig, ready for restart, and the scans, oldest first.
func waitingPair(t *testing.T) (*rig, []Document) {
	t.Helper()
	test := newRig(t, true)
	test.perform(t, settings.OneGreyPage)
	test.waitFor(t, "a scan", func(status Status) bool { return len(status.Review) == 1 })
	test.advance(time.Second)
	test.perform(t, settings.OneGreyPage)
	review := test.waitFor(t, "two scans", func(status Status) bool { return len(status.Review) == 2 }).Review
	test.stop()
	pair := []Document{review[1], review[0]}
	for _, document := range pair {
		found, err := test.station.disk.find(document.ID)
		if err != nil {
			t.Fatal(err)
		}
		waiting := found.document
		waiting.State = Waiting
		if err := writeInfo(found.folder, waiting); err != nil {
			t.Fatal(err)
		}
	}
	return test, pair
}

// restart runs a new station on the rig's data folder and clock, sending to destination,
// with upload rounds every retry and whenever asked.
func (r *rig) restart(t *testing.T, destination *fakeDestination, retry time.Duration) *Station {
	t.Helper()
	restarted, err := New(Config{DataDir: r.directory, Settings: r.settings, Driver: r.scanner, Destination: destination,
		Now: r.now, ButtonPoll: testTick, Retry: retry, Reopen: testTick})
	if err != nil {
		t.Fatal(err)
	}
	run(t, restarted)
	return restarted
}

// run runs station until the test ends, and waits for it to stop before the test's files
// are removed, so no write of its outlives them. The function it returns stops it early,
// the same way.
func run(t *testing.T, station *Station) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); station.Run(ctx) }()
	stop = func() { cancel(); <-stopped }
	t.Cleanup(stop)
	return stop
}

func waitOn(t *testing.T, box *Station, what string, done func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		if status := box.Status(); done(status) {
			return status
		}
		time.Sleep(testTick)
	}
	t.Fatalf("timed out waiting for %s; status %+v", what, box.Status())
	return Status{}
}

func (f *fakeDestination) Confirm(_ context.Context, receipt string) (Delivery, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.confirming == nil {
		return Delivery{DocumentID: storedDocumentID, Listed: true}, nil
	}
	return f.confirming(receipt)
}

func (f *fakeDestination) answer(delivery Delivery, err error) {
	f.mutex.Lock()
	f.confirming = func(string) (Delivery, error) { return delivery, err }
	f.mutex.Unlock()
}

func (f *fakeDestination) Upload(_ context.Context, title, fileName, path string, scanned time.Time) (string, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.attempts++
	if f.attemptsFor == nil {
		f.attemptsFor = map[string]int{}
	}
	f.attemptsFor[fileName]++
	if f.failing {
		return "", errors.New("Paperless can't be reached")
	}
	if f.denying {
		return "", fmt.Errorf("Paperless refused the token; %w", ErrDenied)
	}
	if fileName == f.rejecting {
		return "", fmt.Errorf("Paperless refused the file (400 Bad Request); %w", ErrRejected)
	}
	if f.dropping {
		return "", fmt.Errorf("%w: the connection broke", ErrUnclear)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	f.uploads = append(f.uploads, upload{title: title, fileName: fileName, scanned: scanned, content: content})
	receipt := "task-" + fileName
	if f.received == nil {
		f.received = map[string]string{}
	}
	f.received[fileName] = receipt
	if f.losing {
		return "", fmt.Errorf("%w: no answer in time", ErrUnclear)
	}
	return receipt, nil
}

func (f *fakeDestination) Check(context.Context) error { return nil }

func (f *fakeDestination) count() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return len(f.uploads)
}

func (f *fakeDestination) fail(failing bool) {
	f.mutex.Lock()
	f.failing = failing
	f.mutex.Unlock()
}

type rig struct {
	station     *Station
	scanner     *scanner.Fake
	destination *fakeDestination
	settings    *settings.Store
	clockOffset time.Duration
	clockMutex  sync.Mutex
	// stop ends the station and waits until it has stopped.
	stop      func()
	directory string
	// free is the space the rig's disk reports.
	free atomic.Int64
}

// plentyOfSpace is what the rig's disk reports free unless a test says otherwise.
const plentyOfSpace = 100 << 30

func (r *rig) freeSpace(string) (int64, error) { return r.free.Load(), nil }

func newRig(t *testing.T, review bool) *rig {
	t.Helper()
	return newRigWith(t, review, 0)
}

// newRigWith is newRig with a page limit for documents; 0 keeps the default.
func newRigWith(t *testing.T, review bool, maxPages int) *rig {
	t.Helper()
	directory := t.TempDir()
	store, err := settings.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set("resolution_dpi", "150"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set("review_before_sending", map[bool]string{true: "true", false: "false"}[review]); err != nil {
		t.Fatal(err)
	}
	test := &rig{scanner: scanner.NewFake(), destination: &fakeDestination{}, settings: store, directory: directory}
	test.free.Store(plentyOfSpace)
	test.station, err = New(Config{
		DataDir: directory, Settings: store, Driver: test.scanner, Destination: test.destination,
		Now: test.now, ButtonPoll: testTick, Retry: 20 * testTick, Reopen: testTick,
		FreeSpace: test.freeSpace, MaxPages: maxPages,
	})
	if err != nil {
		t.Fatal(err)
	}
	test.stop = run(t, test.station)
	test.waitFor(t, "the scanner to be found", func(status Status) bool { return status.Scanner != "" })
	return test
}

func (r *rig) now() time.Time {
	r.clockMutex.Lock()
	defer r.clockMutex.Unlock()
	return time.Now().Add(r.clockOffset)
}

func (r *rig) advance(by time.Duration) {
	r.clockMutex.Lock()
	r.clockOffset += by
	r.clockMutex.Unlock()
}

func (r *rig) waitFor(t *testing.T, what string, done func(Status) bool) Status {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		status := r.station.Status()
		if done(status) {
			return status
		}
		time.Sleep(testTick)
	}
	t.Fatalf("timed out waiting for %s; status %+v", what, r.station.Status())
	return Status{}
}

func (r *rig) perform(t *testing.T, action settings.Action) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for r.station.Perform(action) == ErrBusy {
		if time.Now().After(deadline) {
			t.Fatal("scanner stayed busy")
		}
		time.Sleep(testTick)
	}
}

func TestOnePageWaitsForReviewThenUploadsUnderItsName(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.OneGreyPage)
	status := test.waitFor(t, "a scan to review", func(status Status) bool { return len(status.Review) == 1 })
	document := status.Review[0]
	if !strings.HasPrefix(document.Name, "Scan ") || document.Pages != 1 || document.Colour != greyscale {
		t.Fatalf("review document %+v", document)
	}
	time.Sleep(10 * testTick)
	if test.destination.count() != 0 {
		t.Fatal("a scan was sent before review")
	}
	if err := test.station.Rename(document.ID, sampleTitle); err != nil {
		t.Fatal(err)
	}
	if err := test.station.Send(document.ID); err != nil {
		t.Fatal(err)
	}
	status = test.waitFor(t, "the upload", func(status Status) bool { return len(status.Sent) == 1 })
	test.destination.mutex.Lock()
	sent := test.destination.uploads[0]
	test.destination.mutex.Unlock()
	if sent.title != sampleTitle || sent.fileName != "scanbox-"+document.ID+".pdf" || !strings.HasPrefix(string(sent.content), "%PDF-1.4") {
		t.Fatalf("uploaded %q as %q", sent.title, sent.fileName)
	}
	if !sent.scanned.Equal(document.Created) {
		t.Fatalf("uploaded as scanned at %v, but the scan was made at %v", sent.scanned, document.Created)
	}
	if status.Sent[0].Task == "" || status.Sent[0].SentAt.IsZero() || len(status.Review) != 0 {
		t.Fatalf("after sending: %+v", status)
	}
}

// folderTotal adds up the files a scan's folder holds, as the disk has them.
func folderTotal(t *testing.T, folder string) int64 {
	t.Helper()
	entries, err := os.ReadDir(folder)
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		total += info.Size()
	}
	return total
}

// The space scans use comes from the index, without a walk through every folder, and
// follows each change: a rename rewrites the facts, a deletion frees it all.
func TestUsageIsWhatTheKeptScansHold(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.OneGreyPage)
	document := test.waitFor(t, "a scan to review", func(status Status) bool { return len(status.Review) == 1 }).Review[0]
	path, err := test.station.PDFPath(document.ID)
	if err != nil {
		t.Fatal(err)
	}
	folder := filepath.Dir(path)
	if used, _ := test.station.Usage(); used != folderTotal(t, folder) || used == 0 {
		t.Fatalf("Usage says %d bytes, the scan's folder holds %d", used, folderTotal(t, folder))
	}
	if err := test.station.Rename(document.ID, strings.Repeat("A much longer name ", 5)); err != nil {
		t.Fatal(err)
	}
	if used, _ := test.station.Usage(); used != folderTotal(t, folder) {
		t.Fatalf("after a rename Usage says %d bytes, the folder holds %d", used, folderTotal(t, folder))
	}
	if err := test.station.Delete(document.ID); err != nil {
		t.Fatal(err)
	}
	if used, _ := test.station.Usage(); used != 0 {
		t.Fatalf("with no scans kept Usage says %d bytes", used)
	}
}

func TestWithoutReviewAScanGoesStraightToPaperless(t *testing.T) {
	t.Parallel()
	test := newRig(t, false)
	test.perform(t, settings.OneColourPage)
	status := test.waitFor(t, "the upload", func(status Status) bool { return len(status.Sent) == 1 })
	if status.Sent[0].Colour != colourful || len(status.Review) != 0 {
		t.Fatalf("status %+v", status)
	}
}

func TestMixedMultiPageDocument(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.AddColourPage)
	test.perform(t, settings.AddGreyPage)
	test.perform(t, settings.AddGreyPage)
	test.waitFor(t, "three open pages", func(status Status) bool { return status.OpenPages == 3 })
	if err := test.station.RemoveLastPage(); err != nil {
		t.Fatal(err)
	}
	test.waitFor(t, "the last page removed", func(status Status) bool { return status.OpenPages == 2 })
	if path, err := test.station.OpenThumbnailPath(2); err != nil || !fileExists(path) {
		t.Fatalf("thumbnail of page 2: %q %v", path, err)
	}
	if path, err := test.station.OpenPreviewPath(2); err != nil || !fileExists(path) {
		t.Fatalf("preview of open page 2: %q %v", path, err)
	}
	test.perform(t, settings.FinishDocument)
	status := test.waitFor(t, "the finished document", func(status Status) bool { return len(status.Review) == 1 })
	document := status.Review[0]
	if document.Pages != 2 || document.Colour != mixed || status.OpenPages != 0 {
		t.Fatalf("document %+v, open pages %d", document, status.OpenPages)
	}
	path, err := test.station.PDFPath(document.ID)
	if err != nil {
		t.Fatal(err)
	}
	checkPDF(t, path, 2)
	for page := 1; page <= 2; page++ {
		if thumbnail, err := test.station.ThumbnailPath(document.ID, page); err != nil || !fileExists(thumbnail) {
			t.Fatalf("thumbnail %d: %v", page, err)
		}
		preview, err := test.station.PreviewPath(document.ID, page)
		if err != nil || !strings.HasSuffix(preview, ".preview.jpg") || !fileExists(preview) {
			t.Fatalf("preview %d: %q %v", page, preview, err)
		}
	}
	if _, err := test.station.PreviewPath(document.ID, 3); !errors.Is(err, ErrNotFound) {
		t.Fatal("a page past the end was found")
	}
	preview, _ := test.station.PreviewPath(document.ID, 1)
	if err := os.Remove(preview); err != nil {
		t.Fatal(err)
	}
	if fallback, err := test.station.PreviewPath(document.ID, 1); err != nil || strings.HasSuffix(fallback, ".preview.jpg") {
		t.Fatalf("an older scan without a preview should fall back to its thumbnail: %q %v", fallback, err)
	}
}

func TestFinishWithoutPagesDoesNothing(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.FinishDocument)
	time.Sleep(10 * testTick)
	status := test.station.Status()
	if len(status.Review) != 0 || status.ScanError != "" {
		t.Fatalf("status %+v", status)
	}
}

func TestForgottenDocumentIsFinished(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.AddGreyPage)
	test.waitFor(t, "an open page", func(status Status) bool { return status.OpenPages == 1 })
	time.Sleep(10 * testTick)
	if len(test.station.Status().Review) != 0 {
		t.Fatal("finished too early")
	}
	test.advance(11 * time.Minute)
	test.waitFor(t, "the forgotten document", func(status Status) bool { return len(status.Review) == 1 && status.OpenPages == 0 })
}

func TestFailedUploadWaitsAndRetries(t *testing.T) {
	t.Parallel()
	test := newRig(t, false)
	test.destination.fail(true)
	test.perform(t, settings.OneGreyPage)
	status := test.waitFor(t, "a failed upload", func(status Status) bool {
		return len(status.Waiting) == 1 && status.Waiting[0].LastError != "" && status.UploadError != ""
	})
	if len(status.Sent) != 0 {
		t.Fatal("sent despite the failure")
	}
	if !status.Waiting[0].AttemptedAt.IsZero() {
		t.Fatal("an upload refused outright should leave nothing to look for")
	}
	test.destination.fail(false)
	test.waitFor(t, "the retry", func(status Status) bool {
		return len(status.Sent) == 1 && len(status.Waiting) == 0 && status.UploadError == ""
	})
}

func TestAnAcceptedScanIsInPaperlessOnlyOnceStored(t *testing.T) {
	t.Parallel()
	test := newRig(t, false)
	test.destination.answer(Delivery{Listed: true}, nil)
	test.perform(t, settings.OneGreyPage)
	status := test.waitFor(t, "the upload to be accepted", func(status Status) bool { return len(status.Submitted) == 1 })
	accepted := status.Submitted[0]
	if accepted.Task == "" || accepted.SubmittedAt.IsZero() || !accepted.SentAt.IsZero() || accepted.DocumentID != 0 {
		t.Fatalf("an accepted scan: %+v", accepted)
	}
	time.Sleep(30 * testTick)
	if len(test.station.Status().Sent) != 0 {
		t.Fatal("counted as stored while Paperless was still on it")
	}
	if _, err := test.settings.Set("keep_sent_days", "7"); err != nil {
		t.Fatal(err)
	}
	test.advance(8 * hoursPerDay * time.Hour)
	test.station.SendWaiting()
	time.Sleep(30 * testTick)
	if _, err := test.station.PDFPath(accepted.ID); err != nil {
		t.Fatal("8 days after Paperless accepted it, and never said it stored it, the scan was deleted")
	}
	test.destination.answer(Delivery{DocumentID: storedDocumentID, Listed: true}, nil)
	test.station.SendWaiting()
	status = test.waitFor(t, "the confirmation", func(status Status) bool { return len(status.Sent) == 1 })
	if stored := status.Sent[0]; stored.DocumentID != storedDocumentID || stored.SentAt.IsZero() || len(status.Submitted) != 0 {
		t.Fatalf("a stored scan: %+v", stored)
	}
}

func TestAFailedTaskKeepsTheScanWithItsReason(t *testing.T) {
	t.Parallel()
	test := newRig(t, false)
	if _, err := test.settings.Set("keep_sent_days", "7"); err != nil {
		t.Fatal(err)
	}
	const reason = "Paperless couldn't store it (Unsupported mime type)"
	test.destination.answer(Delivery{Failure: reason, Listed: true}, nil)
	test.perform(t, settings.OneGreyPage)
	failed := test.waitFor(t, "the failure", func(status Status) bool { return len(status.Failed) == 1 }).Failed[0]
	if failed.LastError != reason || failed.DocumentID != 0 {
		t.Fatalf("a failed scan: %+v", failed)
	}
	test.advance(400 * hoursPerDay * time.Hour)
	test.station.SendWaiting()
	time.Sleep(20 * testTick)
	if len(test.station.Status().Failed) != 1 {
		t.Fatal("a scan Paperless didn't store was deleted")
	}
	if path, err := test.station.PDFPath(failed.ID); err != nil || !fileExists(path) {
		t.Fatalf("the failed scan's PDF: %v", err)
	}
}

func TestRetentionCountsFromTheConfirmation(t *testing.T) {
	t.Parallel()
	test := newRig(t, false)
	if _, err := test.settings.Set("keep_sent_days", "7"); err != nil {
		t.Fatal(err)
	}
	test.destination.answer(Delivery{Listed: true}, nil)
	test.perform(t, settings.OneGreyPage)
	test.waitFor(t, "the upload to be accepted", func(status Status) bool { return len(status.Submitted) == 1 })
	test.advance(30 * hoursPerDay * time.Hour)
	test.destination.answer(Delivery{DocumentID: storedDocumentID, Listed: true}, nil)
	test.station.SendWaiting()
	test.waitFor(t, "the confirmation", func(status Status) bool { return len(status.Sent) == 1 })
	test.station.SendWaiting()
	time.Sleep(20 * testTick)
	if len(test.station.Status().Sent) != 1 {
		t.Fatal("a scan confirmed just now was deleted, counting from when it was accepted")
	}
	test.advance(8 * hoursPerDay * time.Hour)
	test.station.SendWaiting()
	test.waitFor(t, "the old copy to go", func(status Status) bool { return len(status.Sent) == 0 })
}

func TestAnUploadPaperlessNeverListsFailsAfterAWhile(t *testing.T) {
	t.Parallel()
	test := newRig(t, false)
	test.destination.answer(Delivery{}, nil)
	test.perform(t, settings.OneGreyPage)
	test.waitFor(t, "the upload to be accepted", func(status Status) bool { return len(status.Submitted) == 1 })
	time.Sleep(20 * testTick)
	if len(test.station.Status().Submitted) != 1 {
		t.Fatal("an upload not listed yet was given up at once")
	}
	test.advance(defaultConfirmWait + time.Minute)
	test.station.SendWaiting()
	failed := test.waitFor(t, "the upload to be given up", func(status Status) bool { return len(status.Failed) == 1 }).Failed[0]
	if failed.LastError != notListed {
		t.Fatalf("the reason: %q", failed.LastError)
	}
}

func TestAProblemAskingLeavesScansAccepted(t *testing.T) {
	t.Parallel()
	test := newRig(t, false)
	refused := errors.New("the token's user may not view tasks")
	test.destination.answer(Delivery{}, refused)
	test.perform(t, settings.OneGreyPage)
	status := test.waitFor(t, "the problem", func(status Status) bool { return len(status.Submitted) == 1 && status.ConfirmError != nil })
	if !errors.Is(status.ConfirmError, refused) {
		t.Fatalf("the problem: %v", status.ConfirmError)
	}
	test.advance(defaultConfirmWait + time.Hour)
	test.station.SendWaiting()
	time.Sleep(20 * testTick)
	if status := test.station.Status(); len(status.Submitted) != 1 || len(status.Failed) != 0 {
		t.Fatalf("a scan was given up because Paperless couldn't be asked: %+v", status)
	}
	test.destination.answer(Delivery{DocumentID: storedDocumentID, Listed: true}, nil)
	test.station.SendWaiting()
	test.waitFor(t, "the confirmation", func(status Status) bool { return len(status.Sent) == 1 && status.ConfirmError == nil })
}

func TestScansSentBeforeConfirmationAreConfirmed(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.OneGreyPage)
	document := test.waitFor(t, "a scan", func(status Status) bool { return len(status.Review) == 1 }).Review[0]
	test.stop()
	// What an older version left: counted as sent when Paperless answered, with no document ID.
	found, err := test.station.disk.find(document.ID)
	if err != nil {
		t.Fatal(err)
	}
	older := found.document
	older.State, older.Task, older.SentAt = Sent, "older-task", older.Created
	if err := writeInfo(found.folder, older); err != nil {
		t.Fatal(err)
	}
	destination := &fakeDestination{}
	destination.answer(Delivery{Listed: true}, nil)
	restarted, err := New(Config{DataDir: test.directory, Settings: test.settings, Driver: test.scanner, Destination: destination, Retry: testTick})
	if err != nil {
		t.Fatal(err)
	}
	status := restarted.Status()
	if len(status.Sent) != 0 || len(status.Submitted) != 1 || !status.Submitted[0].SubmittedAt.Equal(older.SentAt) {
		t.Fatalf("an older sent scan after the start: %+v", status)
	}
	run(t, restarted)
	destination.answer(Delivery{DocumentID: storedDocumentID, Listed: true}, nil)
	deadline := time.Now().Add(waitLimit)
	for len(restarted.Status().Sent) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the older scan was never confirmed")
		}
		time.Sleep(testTick)
	}
	if confirmed := restarted.Status().Sent[0]; confirmed.DocumentID != storedDocumentID || confirmed.Task != "older-task" {
		t.Fatalf("the older scan once confirmed: %+v", confirmed)
	}
}

func TestAnUploadWhoseAnswerWasLostIsNotSentAgain(t *testing.T) {
	t.Parallel()
	test := newRig(t, false)
	test.destination.change(func(destination *fakeDestination) { destination.losing = true })
	test.perform(t, settings.OneGreyPage)
	lost := test.waitFor(t, "an unclear upload", func(status Status) bool {
		return len(status.Waiting) == 1 && !status.Waiting[0].AttemptEnded.IsZero()
	}).Waiting[0]
	if !strings.Contains(lost.LastError, ErrUnclear.Error()) {
		t.Fatalf("the reason: %q", lost.LastError)
	}
	test.station.SendWaiting()
	stored := test.waitFor(t, "the upload to be found", func(status Status) bool { return len(status.Sent) == 1 }).Sent[0]
	if tries := test.destination.tries(); tries != 1 {
		t.Fatalf("an upload Paperless already had was sent %d times", tries)
	}
	if stored.Task != "task-"+lost.UploadName() || !stored.AttemptedAt.IsZero() || !stored.AttemptEnded.IsZero() {
		t.Fatalf("the found upload: %+v", stored)
	}
}

func TestAnUploadThatNeverArrivedIsSentAgainAfterTheWait(t *testing.T) {
	t.Parallel()
	test := newRig(t, false)
	test.destination.change(func(destination *fakeDestination) { destination.dropping = true })
	test.perform(t, settings.OneGreyPage)
	test.waitFor(t, "an unclear upload", func(status Status) bool {
		return len(status.Waiting) == 1 && !status.Waiting[0].AttemptEnded.IsZero()
	})
	test.destination.change(func(destination *fakeDestination) { destination.dropping = false })
	for range 3 {
		test.station.SendWaiting()
		time.Sleep(10 * testTick)
	}
	if tries := test.destination.tries(); tries != 1 {
		t.Fatalf("sent again %d times before Paperless had time to list it", tries-1)
	}
	if status := test.station.Status(); status.UploadError != "" {
		t.Fatalf("waiting to settle is not a failure: %q", status.UploadError)
	}
	test.advance(defaultSettleWait + time.Minute)
	test.station.SendWaiting()
	test.waitFor(t, "the upload sent again", func(status Status) bool { return len(status.Sent) == 1 })
	if tries := test.destination.tries(); tries != 2 {
		t.Fatalf("%d tries, want 2", tries)
	}
}

func TestAStopRightAfterPaperlessAnsweredDoesNotUploadAgain(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.OneGreyPage)
	document := test.waitFor(t, "a scan", func(status Status) bool { return len(status.Review) == 1 }).Review[0]
	test.stop()
	// What a stop between Paperless's answer and writing it down leaves: the try marked as
	// begun, nothing more. Paperless has the upload.
	found, err := test.station.disk.find(document.ID)
	if err != nil {
		t.Fatal(err)
	}
	tried := found.document
	tried.State, tried.AttemptedAt = Waiting, tried.Created
	if err := writeInfo(found.folder, tried); err != nil {
		t.Fatal(err)
	}
	destination := &fakeDestination{received: map[string]string{document.UploadName(): "task-before-the-stop"}}
	restarted, err := New(Config{DataDir: test.directory, Settings: test.settings, Driver: test.scanner, Destination: destination, Retry: testTick})
	if err != nil {
		t.Fatal(err)
	}
	run(t, restarted)
	deadline := time.Now().Add(waitLimit)
	for len(restarted.Status().Sent) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the upload from before the stop was never found")
		}
		time.Sleep(testTick)
	}
	if stored := restarted.Status().Sent[0]; stored.Task != "task-before-the-stop" || destination.tries() != 0 {
		t.Fatalf("after the restart: %+v, %d uploads", stored, destination.tries())
	}
}

func TestARefusedScanIsSetAsideAndTheOthersGo(t *testing.T) {
	t.Parallel()
	test, pair := waitingPair(t)
	refused, next := pair[0], pair[1]
	destination := &fakeDestination{rejecting: refused.UploadName()}
	box := test.restart(t, destination, 20*testTick)
	status := waitOn(t, box, "the round", func(status Status) bool { return len(status.Failed) == 1 && len(status.Sent) == 1 })
	if status.Sent[0].ID != next.ID || status.Failed[0].ID != refused.ID {
		t.Fatalf("the scan behind the refused one should be sent: %+v", status)
	}
	if reason := status.Failed[0].LastError; !strings.Contains(reason, "400") || !strings.Contains(reason, ErrRejected.Error()) {
		t.Fatalf("the reason: %q", reason)
	}
	for range 3 {
		test.advance(longestPause)
		box.SendWaiting()
		time.Sleep(10 * testTick)
	}
	if tries := destination.triesOf(refused.UploadName()); tries != 1 {
		t.Fatalf("the refused scan was uploaded %d times", tries)
	}
	destination.change(func(destination *fakeDestination) { destination.rejecting = "" })
	if err := box.Retry(refused.ID); err != nil {
		t.Fatal(err)
	}
	waitOn(t, box, "the retried scan to be sent", func(status Status) bool { return len(status.Sent) == 2 && len(status.Failed) == 0 })
}

func TestARefusedTokenStopsTheRoundAndPausesUploads(t *testing.T) {
	t.Parallel()
	test, _ := waitingPair(t)
	destination := &fakeDestination{denying: true}
	box := test.restart(t, destination, 10*time.Second)
	status := waitOn(t, box, "the refusal", func(status Status) bool { return status.UploadError != "" })
	if !strings.Contains(status.UploadError, ErrDenied.Error()) || len(status.Waiting) != 2 || destination.tries() != 1 {
		t.Fatalf("one refused token should stop the round: %d tries, %+v", destination.tries(), status)
	}
	for _, waiting := range status.Waiting {
		if waiting.Tries != 0 || !waiting.NextTry.IsZero() {
			t.Fatalf("a refused token is no fault of the scan, which shouldn't wait a pause of its own: %+v", waiting)
		}
	}
	for range 3 {
		box.SendWaiting()
		time.Sleep(10 * testTick)
	}
	if tries := destination.tries(); tries != 1 {
		t.Fatalf("%d uploads during the pause", tries)
	}
	test.advance(time.Minute)
	box.SendWaiting()
	waitOn(t, box, "a try after the pause", func(Status) bool { return destination.tries() == 2 })
	destination.change(func(destination *fakeDestination) { destination.denying = false })
	box.RetryNow()
	waitOn(t, box, "both sent, once Send now is pressed", func(status Status) bool { return len(status.Sent) == 2 && status.UploadError == "" })
}

func TestAScanThatCameToNothingWaitsLongerEachTime(t *testing.T) {
	t.Parallel()
	test := newRig(t, false)
	test.destination.fail(true)
	test.perform(t, settings.OneGreyPage)
	first := test.waitFor(t, "a failed upload", func(status Status) bool { return len(status.Waiting) == 1 && status.Waiting[0].Tries == 1 }).Waiting[0]
	if pause := first.NextTry.Sub(test.now()); pause <= 0 || pause > test.station.config.Retry {
		t.Fatalf("the first pause: %v", pause)
	}
	tries := test.destination.tries()
	for range 3 {
		test.station.SendWaiting()
		time.Sleep(2 * testTick)
	}
	if test.destination.tries() > tries+1 {
		t.Fatal("the scan was sent again and again during its pause")
	}
	test.advance(time.Minute)
	test.station.SendWaiting()
	second := test.waitFor(t, "another try", func(status Status) bool { return status.Waiting[0].Tries == 2 }).Waiting[0]
	if pause := second.NextTry.Sub(test.now()); pause <= test.station.config.Retry {
		t.Fatalf("the second pause should be longer than the first: %v", pause)
	}
	if test.station.pauseAfter(1) != test.station.config.Retry || test.station.pauseAfter(2) != 2*test.station.config.Retry || test.station.pauseAfter(64) != longestPause {
		t.Fatal("pauses should double, up to an hour")
	}
}

func TestAWaitingScanWithoutItsPDFFails(t *testing.T) {
	t.Parallel()
	test, pair := waitingPair(t)
	path, err := test.station.PDFPath(pair[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	box := test.restart(t, &fakeDestination{}, 20*testTick)
	status := waitOn(t, box, "the round", func(status Status) bool { return len(status.Failed) == 1 && len(status.Sent) == 1 })
	if status.Failed[0].ID != pair[0].ID || status.Failed[0].LastError != errNoPDF.Error() {
		t.Fatalf("a scan without its PDF: %+v", status.Failed[0])
	}
}

func TestUnderTheReserveAPressScansNothingAndSaysWhy(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	reserve := int64(test.settings.Get().ReserveMB) * megabyte
	test.free.Store(reserve - 50*megabyte)
	scans := test.scanner.ScanCount()
	test.perform(t, settings.OneGreyPage)
	status := test.waitFor(t, "the refusal", func(status Status) bool { return status.ScanError != "" })
	if test.scanner.ScanCount() != scans {
		t.Fatal("the scanner scanned although the reserve was reached")
	}
	for _, piece := range []string{"150 MB is free", "less than the 200 MB kept free", makeRoom} {
		if !strings.Contains(status.ScanError, piece) || !strings.Contains(status.LowSpace, piece) {
			t.Errorf("the page should say %q: %q / %q", piece, status.ScanError, status.LowSpace)
		}
	}
	test.free.Store(3 * reserve)
	if low := test.station.Status().LowSpace; !strings.Contains(low, "only 600 MB is free") || strings.Contains(low, "stopped") {
		t.Fatalf("under five times the reserve, a warning, while scanning goes on: %q", low)
	}
	test.perform(t, settings.OneGreyPage)
	test.waitFor(t, "a scan with room again", func(status Status) bool { return len(status.Review) == 1 && status.ScanError == "" })
	test.free.Store(plentyOfSpace)
	if low := test.station.Status().LowSpace; low != "" {
		t.Fatalf("with plenty of space, no warning: %q", low)
	}
}

func TestADocumentStopsAtItsPageLimit(t *testing.T) {
	t.Parallel()
	test := newRigWith(t, true, 2)
	test.perform(t, settings.AddGreyPage)
	test.perform(t, settings.AddGreyPage)
	test.waitFor(t, "two open pages", func(status Status) bool { return status.OpenPages == 2 })
	scans := test.scanner.ScanCount()
	test.perform(t, settings.AddColourPage)
	status := test.waitFor(t, "the refusal", func(status Status) bool { return status.ScanError != "" })
	if test.scanner.ScanCount() != scans || status.OpenPages != 2 {
		t.Fatal("a page past the limit was scanned")
	}
	if !strings.Contains(status.ScanError, "already has 2 pages") || !strings.Contains(status.ScanError, "Finish it, then start another") {
		t.Fatalf("the advice: %q", status.ScanError)
	}
	test.perform(t, settings.OneGreyPage)
	test.waitFor(t, "a single page still scans", func(status Status) bool { return len(status.Review) == 1 })
	if defaultMaxPages != 200 {
		t.Fatal("a document holds up to 200 pages")
	}
}

func TestProblemsWithFilesShowNoPath(t *testing.T) {
	t.Parallel()
	full := &fs.PathError{Op: "write", Path: "/data/scans/2026/11-2026/12-11-2026/x.partial/document.pdf", Err: syscall.ENOSPC}
	if message := plainError(fmt.Errorf("writing the document: %w", full)); message != errDiskFull.Error() {
		t.Errorf("a full disk: %q", message)
	}
	denied := &fs.PathError{Op: "open", Path: "/data/scans/x/info.json", Err: syscall.EACCES}
	if message := plainError(denied); strings.Contains(message, "/data") || !strings.Contains(message, "permission denied") {
		t.Errorf("a file Scan Box may not open: %q", message)
	}
	other := errors.New("the scanner is busy")
	if plainError(other) != other.Error() {
		t.Error("other problems keep their words")
	}
}

func TestABusyScannerIsCalledBusy(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.scanner.SetBusy(true)
	test.perform(t, settings.OneGreyPage)
	status := test.waitFor(t, "the refusal", func(status Status) bool { return status.ScanError != "" })
	if !strings.Contains(status.ScanError, scanner.ErrBusy.Error()) || strings.Contains(status.ScanError, scanner.ErrNoScanner.Error()) {
		t.Fatalf("a busy scanner should be called busy: %q", status.ScanError)
	}
}

// During a scan one more press waits its turn, as the scanner's own buttons do, and the
// status says so: the next sheet is often in place before the scanner has quite finished.
// A press beyond that, and removing the last page, are refused.
func TestDuringAScanOnePressWaitsItsTurn(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.scanner.SetDelay(time.Second)
	test.perform(t, settings.AddGreyPage)
	test.waitFor(t, "the scan to start", func(status Status) bool { return status.Scanning })
	if err := test.station.Perform(settings.AddGreyPage); err != nil {
		t.Fatalf("one more page during a scan: %v, want it to wait its turn", err)
	}
	if !test.station.Status().Queued {
		t.Fatal("the status should say a press is waiting")
	}
	if err := test.station.Perform(settings.OneColourPage); !errors.Is(err, ErrBusy) {
		t.Fatalf("a second press during the same scan: %v, want ErrBusy", err)
	}
	if err := test.station.RemoveLastPage(); !errors.Is(err, ErrBusy) {
		t.Fatalf("removing the last page during a scan: %v, want ErrBusy", err)
	}
	test.waitFor(t, "both pages scanned", func(status Status) bool { return status.OpenPages == 2 && !status.Scanning && !status.Queued })
	test.scanner.SetDelay(0)
	if err := test.station.Perform(settings.FinishDocument); err != nil {
		t.Fatalf("finishing once the scanner is free: %v", err)
	}
	status := test.waitFor(t, "the finished document", func(status Status) bool { return len(status.Review) == 1 })
	if status.Review[0].Pages != 2 {
		t.Fatalf("finished %d pages, want the 2 pressed for", status.Review[0].Pages)
	}
}

// deadlineRig is a rig whose scans may take 50 ms, and whose driver counts as stuck 50 ms later.
func deadlineRig(t *testing.T, stuck func()) *rig {
	t.Helper()
	test := newRig(t, true)
	test.stop()
	box, err := New(Config{DataDir: test.directory, Settings: test.settings, Driver: test.scanner, Destination: test.destination,
		Now: test.now, ButtonPoll: testTick, Retry: 20 * testTick, Reopen: testTick, FreeSpace: test.freeSpace,
		ScanDeadline: 50 * time.Millisecond, StuckGrace: 50 * time.Millisecond, Stuck: stuck})
	if err != nil {
		t.Fatal(err)
	}
	test.station, test.stop = box, run(t, box)
	test.waitFor(t, "the scanner to be found", func(status Status) bool { return status.Scanner != "" })
	return test
}

func TestAScanPastItsDeadlineIsStopped(t *testing.T) {
	t.Parallel()
	stuck := atomic.Bool{}
	test := deadlineRig(t, func() { stuck.Store(true) })
	test.scanner.SetDelay(5 * time.Second)
	test.perform(t, settings.OneGreyPage)
	status := test.waitFor(t, "the scan to be stopped", func(status Status) bool { return status.ScanError != "" })
	if !strings.Contains(status.ScanError, "took longer than 50ms and was stopped") || stuck.Load() {
		t.Fatalf("a scan past its deadline: %q, driver counted stuck: %v", status.ScanError, stuck.Load())
	}
	test.scanner.SetDelay(0)
	test.waitFor(t, "the scanner to be found again", func(status Status) bool { return status.Scanner != "" })
	if err := test.station.Health(); err != nil {
		t.Fatalf("after a stopped scan the station is well: %v", err)
	}
}

func TestADriverThatNeverReturnsTurnsHealthBadAndIsStopped(t *testing.T) {
	t.Parallel()
	stuck := make(chan struct{})
	test := deadlineRig(t, func() { close(stuck) })
	release := test.scanner.Hang()
	defer release()
	test.perform(t, settings.OneGreyPage)
	select {
	case <-stuck:
	case <-time.After(waitLimit):
		t.Fatal("a driver that never returns wasn't called stuck")
	}
	if err := test.station.Health(); !errors.Is(err, errScannerStuck) {
		t.Fatalf("health with a stuck driver: %v", err)
	}
}

func TestLoopsThatStopTurnHealthBad(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	if err := test.station.Health(); err != nil {
		t.Fatalf("a running station: %v", err)
	}
	test.stop()
	time.Sleep(10 * testTick)
	test.advance(2 * scannerStillLimit)
	if err := test.station.Health(); !errors.Is(err, errScannerStill) {
		t.Fatalf("a scanner loop that stopped: %v", err)
	}
}

// It reads the program's one log, so it runs alone, not beside the parallel tests.
func TestUploadOutcomesAreLogged(t *testing.T) {
	var written bytes.Buffer
	log.SetOutput(&written)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	test := newRig(t, false)
	test.perform(t, settings.OneGreyPage)
	stored := test.waitFor(t, "the upload", func(status Status) bool { return len(status.Sent) == 1 }).Sent[0]
	test.destination.fail(true)
	test.perform(t, settings.OneGreyPage)
	test.waitFor(t, "a failed upload", func(status Status) bool { return len(status.Waiting) == 1 && status.Waiting[0].Tries == 1 })
	logged := written.String()
	for _, line := range []string{"upload " + stored.ID + ": accepted, task task-" + stored.UploadName(), fmt.Sprintf("upload %s: stored as document %d", stored.ID, storedDocumentID), ": failed, next try at "} {
		if !strings.Contains(logged, line) {
			t.Errorf("the log lacks %q:\n%s", line, logged)
		}
	}
}

// holding is a destination whose uploads wait until released, as a slow one does.
type holding struct {
	*fakeDestination
	started, release chan struct{}
}

func (h holding) Upload(ctx context.Context, title, fileName, path string, scanned time.Time) (string, error) {
	close(h.started)
	<-h.release
	return h.fakeDestination.Upload(ctx, title, fileName, path, scanned)
}

func TestChangesDuringAnUploadAreKeptOrRefused(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.OneGreyPage)
	document := test.waitFor(t, "a scan", func(status Status) bool { return len(status.Review) == 1 }).Review[0]
	test.stop()
	destination := holding{fakeDestination: &fakeDestination{}, started: make(chan struct{}), release: make(chan struct{})}
	box, err := New(Config{DataDir: test.directory, Settings: test.settings, Driver: test.scanner, Destination: destination,
		Now: test.now, ButtonPoll: testTick, Retry: 20 * testTick, Reopen: testTick, FreeSpace: test.freeSpace})
	if err != nil {
		t.Fatal(err)
	}
	run(t, box)
	box.Send(document.ID)
	<-destination.started
	if waiting := box.Status().Waiting; len(waiting) != 1 || !waiting[0].Sending {
		t.Fatalf("the scan being sent should say so: %+v", waiting)
	}
	if found := box.Search(Query{Clock: test.settings.Get().Clock()}); len(found) != 1 || !found[0].Sending {
		t.Fatal("the Scans page should see it being sent too")
	}
	if err := box.Rename(document.ID, "Gas bill, paid"); err != nil {
		t.Fatal(err)
	}
	if err := box.Delete(document.ID); !errors.Is(err, ErrSending) {
		t.Fatalf("deleting a scan being sent: %v", err)
	}
	close(destination.release)
	stored := waitOn(t, box, "the upload", func(status Status) bool { return len(status.Sent) == 1 }).Sent[0]
	if stored.Name != "Gas bill, paid" || stored.SentName != document.Name || stored.PaperlessTitle() != document.Name || stored.Sending {
		t.Fatalf("after the upload: name %q, title in Paperless %q", stored.Name, stored.PaperlessTitle())
	}
	destination.mutex.Lock()
	title := destination.uploads[0].title
	destination.mutex.Unlock()
	if title != document.Name {
		t.Fatalf("Paperless got %q", title)
	}
	if err := box.Delete(document.ID); err != nil {
		t.Fatalf("once sent, the scan can be deleted: %v", err)
	}
}

func TestScannerUnpluggedAndPluggedBackIn(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.scanner.SetMissing(true)
	test.perform(t, settings.OneGreyPage)
	test.waitFor(t, "the scanner to be missed", func(status Status) bool { return status.Scanner == "" && status.ScanError != "" })
	test.scanner.SetMissing(false)
	test.waitFor(t, "the scanner to be found again", func(status Status) bool { return status.Scanner != "" })
	test.perform(t, settings.OneGreyPage)
	test.waitFor(t, "a scan after replugging", func(status Status) bool { return len(status.Review) == 1 && status.ScanError == "" })
}

func TestFrontButtonsFollowTheSettings(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.scanner.Press("pdf")
	test.scanner.Press("send")
	test.waitFor(t, "two pages from buttons", func(status Status) bool { return status.OpenPages == 2 })
	test.scanner.Press("pdf_finish")
	status := test.waitFor(t, "the document from buttons", func(status Status) bool { return len(status.Review) == 1 })
	if status.Review[0].Colour != mixed {
		t.Fatalf("PDF should add colour and Send greyscale; got %s", status.Review[0].Colour)
	}
	if _, err := test.settings.Set("button.copy", string(settings.DoNothing)); err != nil {
		t.Fatal(err)
	}
	scans := test.scanner.ScanCount()
	test.scanner.Press("copy")
	time.Sleep(20 * testTick)
	if test.scanner.ScanCount() != scans {
		t.Fatal("a button set to nothing still scanned")
	}
}

func TestRenameDeleteAndUnsafeNames(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.OneGreyPage)
	document := test.waitFor(t, "a scan", func(status Status) bool { return len(status.Review) == 1 }).Review[0]
	if err := test.station.Rename(document.ID, "../escape"); !errors.Is(err, settings.ErrInvalid) {
		t.Fatalf("unsafe name accepted: %v", err)
	}
	for _, id := range []string{"../settings", "20261111-191352/../../x", ""} {
		if _, err := test.station.PDFPath(id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("id %q gave %v", id, err)
		}
	}
	if err := test.station.Delete(document.ID); err != nil {
		t.Fatal(err)
	}
	test.waitFor(t, "the deletion", func(status Status) bool { return len(status.Review) == 0 })
}

func TestOldSentCopiesAreForgottenOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	test := newRig(t, false)
	test.perform(t, settings.OneGreyPage)
	test.waitFor(t, "the upload", func(status Status) bool { return len(status.Sent) == 1 })
	test.advance(400 * hoursPerDay * time.Hour)
	test.station.SendWaiting()
	time.Sleep(20 * testTick)
	if len(test.station.Status().Sent) != 1 {
		t.Fatal("a sent scan was deleted although the setting keeps them always")
	}
	if _, err := test.settings.Set("keep_sent_days", "7"); err != nil {
		t.Fatal(err)
	}
	test.station.SendWaiting()
	test.waitFor(t, "the old copy to go", func(status Status) bool { return len(status.Sent) == 0 })
}

func TestScansAreKeptInEuropeanDateFolders(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.OneGreyPage)
	document := test.waitFor(t, "a scan", func(status Status) bool { return len(status.Review) == 1 }).Review[0]
	local := document.Created.In(test.settings.Get().Clock().Location)
	folder := filepath.Join(test.directory, scansFolder, local.Format("2006"), local.Format("01-2006"), local.Format("02-01-2006"), document.ID)
	if !fileExists(filepath.Join(folder, pdfFile)) {
		t.Fatalf("no PDF in %s", folder)
	}
	if !strings.HasPrefix(document.ID, local.Format("02-01-2006_")) || !strings.HasPrefix(document.Name, "Scan "+local.Format("02-01-2006")) {
		t.Fatalf("ID %q or name %q not European", document.ID, document.Name)
	}
	if err := test.station.Delete(document.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(test.directory, scansFolder, local.Format("2006"))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty date folders were left behind")
	}
}

func TestOlderScansMoveIntoDateFolders(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	london, _ := time.LoadLocation("Europe/London")
	created := time.Date(2026, 11, 11, 19, 13, 52, 0, london)
	for place, id := range map[string]string{"review": "20261111-191352", "sent": "20261111-191400"} {
		folder := filepath.Join(directory, place, id)
		os.MkdirAll(folder, folderPermissions)
		os.WriteFile(filepath.Join(folder, pdfFile), []byte("%PDF-1.4"), filePermissions)
		writeInfo(folder, Document{ID: id, Name: "old " + place, Pages: 1, Created: created})
	}
	store, _ := settings.Open(directory)
	box, err := New(Config{DataDir: directory, Settings: store, Driver: scanner.NewFake(), Destination: &fakeDestination{}})
	if err != nil {
		t.Fatal(err)
	}
	status := box.Status()
	if len(status.Review) != 1 || len(status.Sent) != 1 || status.Review[0].State != ToReview || status.Sent[0].State != Sent {
		t.Fatalf("older scans after the move: %+v", status)
	}
	if !fileExists(filepath.Join(directory, scansFolder, "2026", "11-2026", "11-11-2026", "20261111-191352", pdfFile)) {
		t.Fatal("an older scan isn't in its date folder")
	}
	if _, err := os.Stat(filepath.Join(directory, "review")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the old review folder is still there")
	}
}

func TestSearchByNameDateAndState(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.OneGreyPage)
	test.perform(t, settings.OneColourPage)
	review := test.waitFor(t, "two scans", func(status Status) bool { return len(status.Review) == 2 }).Review
	test.station.Rename(review[0].ID, "Gas bill November")
	test.station.Rename(review[1].ID, "Council tax")
	test.station.Send(review[1].ID)
	test.waitFor(t, "one sent", func(status Status) bool { return len(status.Sent) == 1 })
	london := test.settings.Get().Clock()
	created := review[0].Created.In(london.Location)
	cases := []struct {
		query Query
		want  int
	}{
		{Query{Text: "gas"}, 1},
		{Query{Text: "BILL november"}, 1},
		{Query{Text: "water"}, 0},
		{Query{Text: created.Format("02/01/2006")}, 2},
		{Query{Text: created.Format("02.01.2006")}, 2},
		{Query{Text: created.Format("2006-01-02")}, 2},
		{Query{Text: created.Format("January 2006")}, 2},
		{Query{Text: created.Format("Monday")}, 2},
		{Query{State: Sent}, 1},
		{Query{State: ToReview, Text: "council"}, 0},
		{Query{From: created.Add(time.Hour)}, 0},
		{Query{Until: created.Add(time.Hour)}, 2},
	}
	for _, example := range cases {
		example.query.Clock = london
		if got := len(test.station.Search(example.query)); got != example.want {
			t.Errorf("Search(%+v) found %d, want %d", example.query, got, example.want)
		}
	}
	months := test.station.Months()
	if len(months) != 1 || months[0].Count != 2 || months[0].Start.Month() != created.Month() {
		t.Fatalf("months %+v", months)
	}
}

func TestOldSentCopiesAreForgotten(t *testing.T) {
	t.Parallel()
	test := newRig(t, false)
	if _, err := test.settings.Set("keep_sent_days", "7"); err != nil {
		t.Fatal(err)
	}
	test.perform(t, settings.OneGreyPage)
	test.waitFor(t, "the upload", func(status Status) bool { return len(status.Sent) == 1 })
	test.advance(8 * hoursPerDay * time.Hour)
	test.station.SendWaiting()
	test.waitFor(t, "the old copy to go", func(status Status) bool { return len(status.Sent) == 0 })
}

func TestOpenDocumentSurvivesARestart(t *testing.T) {
	t.Parallel()
	test := newRig(t, true)
	test.perform(t, settings.AddGreyPage)
	test.waitFor(t, "an open page", func(status Status) bool { return status.OpenPages == 1 })
	test.stop()
	restarted, err := New(Config{DataDir: test.directory, Settings: test.settings, Driver: test.scanner, Destination: test.destination})
	if err != nil {
		t.Fatal(err)
	}
	if pages := restarted.Status().OpenPages; pages != 1 {
		t.Fatalf("open pages after restart: %d", pages)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Size() > 0
}

func checkPDF(t *testing.T, path string, pages int) {
	t.Helper()
	pdfinfo, err := exec.LookPath("pdfinfo")
	if err != nil {
		return
	}
	report, err := exec.Command(pdfinfo, path).CombinedOutput()
	if err != nil || !strings.Contains(string(report), "Pages:") {
		t.Fatalf("pdfinfo: %v\n%s", err, report)
	}
	want := "Pages:           " + string(rune('0'+pages))
	if !strings.Contains(string(report), want) {
		t.Fatalf("wanted %d pages:\n%s", pages, report)
	}
	_ = filepath.Base(path)
}
