// Package station is the scan box itself: it runs the scanner, keeps the open
// document, holds finished scans until they are named and sent, and uploads them.
package station

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"runtime"
	"slices"
	"sort"
	"sync"
	"syscall"
	"time"

	"scanbox/internal/scanner"
	"scanbox/internal/settings"
)

const (
	defaultButtonPoll  = 200 * time.Millisecond
	defaultRetry       = time.Minute
	defaultReopen      = 3 * time.Second
	defaultConfirmWait = time.Hour
	defaultSettleWait  = 5 * time.Minute
	// longestPause bounds the pauses that double after each upload that came to nothing.
	longestPause        = time.Hour
	recentSentCount     = 10
	hoursPerDay         = 24
	waitingJobsCapacity = 1
	// notListed is the reason for an accepted upload the destination never lists.
	notListed = "Paperless accepted it but never listed it among its tasks; check in Paperless whether it is there"
	// defaultMaxPages bounds a document: finishing one reads all its pages at once.
	defaultMaxPages = 200
	// lowSpaceFactor times the reserve is where the status line starts warning.
	lowSpaceFactor = 5
	megabyte       = 1 << 20
	makeRoom       = "Delete old scans in Scans, or free space on the box"
)

var (
	ErrBusy    = errors.New("the scanner is busy; try again when it stops")
	ErrNoPages = errors.New("there is no open document")
	// ErrCantMove is a page moved past either end of the open document.
	ErrCantMove = errors.New("that page can't move further that way")
	// ErrUnclear is an upload that may or may not have arrived: the connection broke or
	// timed out after the document went out, or the destination answered with a fault of
	// its own. It is looked for at the destination before it is sent again.
	ErrUnclear = errors.New("it isn't known whether the upload arrived")
	// ErrRejected is a document the destination refuses as it is. It is set aside as
	// failed, so the documents behind it go on.
	ErrRejected = errors.New("sending it again as it is won't help")
	// ErrDenied is a refusal every document would meet: the credentials, their
	// permissions or the address. The round stops, and uploads pause.
	ErrDenied = errors.New("nothing is sent until that is fixed")
	// errNoPDF is a waiting document whose PDF is gone from the box.
	errNoPDF = errors.New("its PDF is missing from the box, so it can't be sent")
	// errDiskFull is a scan or a document that couldn't be written for want of space.
	errDiskFull = errors.New("the disk is full. " + makeRoom)
	// ErrSending refuses to delete a scan while it is being sent.
	ErrSending = errors.New("this scan is being sent to Paperless right now; it can be deleted once that is done")
	// errSettling holds a resend back while an unclear upload may still turn up.
	errSettling = errors.New("waiting to see whether the last upload arrived before sending it again")
)

// Destination is where finished documents go. Paperless is the first one. Upload hands a
// document scanned at the given time over and returns the destination's receipt; Confirm
// says, from that receipt, what became of it; Find gives the receipt of an earlier upload
// of the same file, if the destination has one.
type Destination interface {
	Upload(ctx context.Context, title, fileName, path string, scanned time.Time) (string, error)
	Confirm(ctx context.Context, receipt string) (Delivery, error)
	Find(ctx context.Context, fileName string) (string, error)
	Check(ctx context.Context) error
}

// Delivery is what a destination says became of a document it accepted. While neither
// DocumentID nor Failure is set, it isn't settled yet.
type Delivery struct {
	// DocumentID is the destination's own number for the document it stored.
	DocumentID int
	// Failure says why the document wasn't stored.
	Failure string
	// Listed says whether the destination knows the receipt at all.
	Listed bool
}

type Config struct {
	DataDir     string
	Settings    *settings.Store
	Driver      scanner.Driver
	Destination Destination
	Now         func() time.Time
	ButtonPoll  time.Duration
	Retry       time.Duration
	Reopen      time.Duration
	// ConfirmWait is how long an accepted upload may stay unknown to the destination
	// before it counts as failed.
	ConfirmWait time.Duration
	// SettleWait is how long after an unclear upload the destination is still asked for
	// it before the document is sent again.
	SettleWait time.Duration
	// FreeSpace says how many bytes are free where the data folder is.
	FreeSpace func(path string) (int64, error)
	// MaxPages is the most pages one document may hold.
	MaxPages int
	// ScanDeadline, when set, replaces the deadline a scan gets from its resolution.
	ScanDeadline time.Duration
	// StuckGrace is how long past its deadline a scan may go on before the driver counts
	// as stuck, and Stuck is called. The program ends itself there, so Docker starts it again.
	StuckGrace time.Duration
	Stuck      func()
}

// Status is everything the Scanner page shows.
type Status struct {
	Scanner  string
	Device   scanner.Details
	Scanning bool
	// Saving says the scanner is done and its page is being stored: at 600 dpi that takes
	// seconds, in which the page isn't shown yet.
	Saving bool
	// Queued says a press waits for the scan under way to end.
	Queued    bool
	ScanError string
	OpenPages int
	// OpenPageNumbers names the open pages, in the document's order.
	OpenPageNumbers []int
	FinishesAt      time.Time
	Review          []Document
	Waiting         []Document
	Submitted       []Document
	Sent            []Document
	Failed          []Document
	UploadError     string
	// ConfirmError is why the destination couldn't be asked about accepted documents.
	ConfirmError error
	// LowSpace says that space is running out, or so short that scanning stops.
	LowSpace string
}

type job struct {
	action     settings.Action
	removeLast bool
}

type Station struct {
	config Config
	// started is when this run began: an upload a stop cut short ended before it.
	started   time.Time
	disk      *disk
	jobs      chan job
	uploadNow chan struct{}

	files sync.Mutex

	mutex       sync.Mutex
	scannerName string
	device      scanner.Details
	scanning    bool
	// saving is the time after a scan while its page is made into pictures and stored.
	saving       bool
	scanError    string
	uploadError  string
	confirmError error
	// denials counts the rounds in a row stopped by ErrDenied; until pausedUntil no upload starts.
	denials     int
	pausedUntil time.Time
	beats       beats
	// sending is the document being uploaded at this moment, if any.
	sending  string
	watchers map[chan struct{}]struct{}

	nextOpen time.Time
}

func New(config Config) (*Station, error) {
	if config.Now == nil {
		config.Now = time.Now
	}
	config.ButtonPoll = orDefault(config.ButtonPoll, defaultButtonPoll)
	config.Retry = orDefault(config.Retry, defaultRetry)
	config.Reopen = orDefault(config.Reopen, defaultReopen)
	config.ConfirmWait = orDefault(config.ConfirmWait, defaultConfirmWait)
	config.SettleWait = orDefault(config.SettleWait, defaultSettleWait)
	config.StuckGrace = orDefault(config.StuckGrace, defaultStuckGrace)
	if config.FreeSpace == nil {
		config.FreeSpace = freeSpace
	}
	if config.MaxPages == 0 {
		config.MaxPages = defaultMaxPages
	}
	started := config.Now()
	station := &Station{
		config:    config,
		started:   started,
		disk:      newDisk(config.DataDir),
		jobs:      make(chan job, waitingJobsCapacity),
		uploadNow: make(chan struct{}, 1),
		watchers:  map[chan struct{}]struct{}{},
		beats:     beats{scanner: started, uploads: started},
	}
	return station, station.disk.prepare(config.Settings.Get().Clock().Location)
}

func orDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

// Run drives the scanner and the uploads until the context ends.
func (s *Station) Run(ctx context.Context) {
	var running sync.WaitGroup
	running.Add(2)
	go func() { defer running.Done(); s.runScanner(ctx) }()
	go func() { defer running.Done(); s.runUploads(ctx) }()
	running.Wait()
}

// Perform queues an action from the page; the scanner's own buttons use the same path.
func (s *Station) Perform(action settings.Action) error {
	return s.queue(job{action: action})
}

func (s *Station) RemoveLastPage() error {
	return s.queue(job{removeLast: true})
}

// MoveOpenPage moves a page of the open document, named by its number, one place earlier
// (offset -1) or later (offset 1). It needn't wait for the scanner: a page scanned
// meanwhile comes after the others, as always.
func (s *Station) MoveOpenPage(number, offset int) error {
	s.files.Lock()
	err := s.disk.moveOpenPage(number, offset)
	s.files.Unlock()
	if err != nil {
		return err
	}
	s.changed()
	return nil
}

// queue hands work to the scanner loop. While a page is being scanned, one more press waits
// its turn, as presses of the scanner's own buttons do, and the status says so: the next
// sheet is often in place before the scanner has quite finished. Removing the last page is
// refused meanwhile, since it would take away the page being scanned.
func (s *Station) queue(work job) error {
	s.mutex.Lock()
	scanning := s.scanning
	s.mutex.Unlock()
	if scanning && work.removeLast {
		log.Print("removing the last page refused: a page is being scanned")
		return ErrBusy
	}
	select {
	case s.jobs <- work:
		s.changed()
		return nil
	default:
		log.Printf("%s refused: a page is being scanned and one more is waiting", work.action)
		return ErrBusy
	}
}

// Watch returns a channel that receives a signal after every change, and a function to stop.
func (s *Station) Watch() (<-chan struct{}, func()) {
	signal := make(chan struct{}, 1)
	s.mutex.Lock()
	s.watchers[signal] = struct{}{}
	s.mutex.Unlock()
	return signal, func() {
		s.mutex.Lock()
		delete(s.watchers, signal)
		s.mutex.Unlock()
	}
}

func (s *Station) changed() {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	for signal := range s.watchers {
		select {
		case signal <- struct{}{}:
		default:
		}
	}
}

// Touch tells everyone watching that something changed, for example a setting.
func (s *Station) Touch() { s.changed() }

func (s *Station) setState(change func()) {
	s.mutex.Lock()
	change()
	s.mutex.Unlock()
	s.changed()
}

func (s *Station) Status() Status {
	s.mutex.Lock()
	status := Status{Scanner: s.scannerName, Device: s.device, Scanning: s.scanning, Saving: s.saving, Queued: len(s.jobs) > 0, ScanError: s.scanError, UploadError: s.uploadError, ConfirmError: s.confirmError}
	sending := s.sending
	s.mutex.Unlock()
	s.files.Lock()
	defer s.files.Unlock()
	numbers, _ := s.disk.openPageNumbers()
	status.OpenPages, status.OpenPageNumbers = len(numbers), numbers
	if last, open := s.disk.lastOpenPageTime(); open {
		status.FinishesAt = last.Add(time.Duration(s.config.Settings.Get().IdleFinishMinutes) * time.Minute)
	}
	status.Review = s.disk.list(ToReview)
	sort.Slice(status.Review, func(i, j int) bool { return status.Review[i].Created.After(status.Review[j].Created) })
	status.Waiting = markSending(s.disk.list(Waiting), sending)
	status.Submitted = s.disk.list(Submitted)
	status.Failed = s.disk.list(Failed)
	sent := s.disk.list(Sent)
	sort.Slice(sent, func(i, j int) bool { return sent[i].SentAt.After(sent[j].SentAt) })
	status.Sent = sent[:min(len(sent), recentSentCount)]
	status.LowSpace = s.lowSpace()
	return status
}

// markSending marks, among documents, the one being uploaded at this moment.
func markSending(documents []Document, sending string) []Document {
	for index := range documents {
		documents[index].Sending = sending != "" && documents[index].ID == sending
	}
	return documents
}

// lowSpace says when less than five times the reserve is free, and, under the reserve,
// that scanning has stopped.
func (s *Station) lowSpace() string {
	free, err := s.config.FreeSpace(s.config.DataDir)
	if err != nil {
		return ""
	}
	if problem := s.roomForScans(free); problem != nil {
		return problem.Error()
	}
	if free < lowSpaceFactor*s.reserve() {
		return fmt.Sprintf("only %s is free on the box. %s", megabytes(free), makeRoom)
	}
	return ""
}

func (s *Station) reserve() int64 { return int64(s.config.Settings.Get().ReserveMB) * megabyte }

// roomForScans refuses a scan when less than the reserve is free.
func (s *Station) roomForScans(free int64) error {
	if free >= s.reserve() {
		return nil
	}
	return fmt.Errorf("only %s is free on the box, less than the %s kept free, so scanning has stopped. %s", megabytes(free), megabytes(s.reserve()), makeRoom)
}

func megabytes(bytes int64) string { return fmt.Sprintf("%d MB", bytes/megabyte) }

// freeSpace asks the file system how much of it is free for the program.
func freeSpace(path string) (int64, error) {
	var stats syscall.Statfs_t
	if err := syscall.Statfs(path, &stats); err != nil {
		return 0, err
	}
	return int64(stats.Bavail) * int64(stats.Bsize), nil
}

// ---- the scanner ----

func (s *Station) runScanner(ctx context.Context) {
	var device scanner.Device
	defer func() { closeDevice(device) }()
	poll := time.NewTicker(s.config.ButtonPoll)
	defer poll.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case work := <-s.jobs:
			device = s.ensureDevice(device)
			device = s.do(ctx, device, work)
		case <-poll.C:
			device = s.ensureDevice(device)
			device = s.pollButton(ctx, device)
			s.finishIfForgotten()
		}
		s.beatScanner()
	}
}

func closeDevice(device scanner.Device) {
	if device != nil {
		device.Close()
	}
}

// ensureDevice opens the scanner if there is none, at most once per reopen interval.
func (s *Station) ensureDevice(device scanner.Device) scanner.Device {
	now := s.config.Now()
	if device != nil || now.Before(s.nextOpen) {
		return device
	}
	opened, err := s.config.Driver.Open()
	if err != nil {
		s.nextOpen = now.Add(s.config.Reopen)
		s.setState(func() { s.scannerName, s.device = "", scanner.Details{} })
		return nil
	}
	s.setState(func() { s.scannerName, s.device = opened.Name(), opened.Details() })
	return opened
}

// lose forgets a scanner that stopped answering, so it is looked for again.
func (s *Station) lose(device scanner.Device, err error) scanner.Device {
	log.Printf("scanner: %v", err)
	closeDevice(device)
	s.nextOpen = s.config.Now().Add(s.config.Reopen)
	s.setState(func() { s.scannerName, s.device = "", scanner.Details{} })
	return nil
}

func (s *Station) pollButton(ctx context.Context, device scanner.Device) scanner.Device {
	if device == nil {
		return nil
	}
	button, pressed, err := device.PressedButton()
	if err != nil {
		return s.lose(device, err)
	}
	if !pressed {
		return device
	}
	action := s.config.Settings.Get().Buttons[settings.Button(button)]
	log.Printf("button %s: %s", button, action)
	return s.do(ctx, device, job{action: action})
}

func (s *Station) do(ctx context.Context, device scanner.Device, work job) scanner.Device {
	if work.removeLast {
		s.report(s.removeLastPage())
		return device
	}
	if err := s.roomToScan(work.action); err != nil {
		s.report(err)
		return device
	}
	switch work.action {
	case settings.FinishDocument:
		s.report(s.finishOpenDocument())
		return device
	case settings.AddColourPage:
		return s.scanInto(ctx, device, scanner.Colour, s.keepOpenPage)
	case settings.AddGreyPage:
		return s.scanInto(ctx, device, scanner.Grey, s.keepOpenPage)
	case settings.OneColourPage:
		return s.scanInto(ctx, device, scanner.Colour, s.finishSinglePage)
	case settings.OneGreyPage:
		return s.scanInto(ctx, device, scanner.Grey, s.finishSinglePage)
	}
	return device
}

// roomToScan refuses an action that would scan, before the scanner moves: when less than
// the reserve is free, or when the open document is already as long as a document may be.
func (s *Station) roomToScan(action settings.Action) error {
	scans := action == settings.AddColourPage || action == settings.AddGreyPage || action == settings.OneColourPage || action == settings.OneGreyPage
	if !scans {
		return nil
	}
	if free, err := s.config.FreeSpace(s.config.DataDir); err == nil {
		if problem := s.roomForScans(free); problem != nil {
			return problem
		}
	}
	if action != settings.AddColourPage && action != settings.AddGreyPage {
		return nil
	}
	s.files.Lock()
	numbers, _ := s.disk.openPageNumbers()
	s.files.Unlock()
	if len(numbers) < s.config.MaxPages {
		return nil
	}
	return fmt.Errorf("this document already has %d pages, the most one can hold. Finish it, then start another", len(numbers))
}

func (s *Station) report(err error) {
	message := ""
	if err != nil && !errors.Is(err, ErrNoPages) {
		log.Printf("scan: %v", err)
		message = plainError(err)
	}
	s.setState(func() { s.scanError = message })
}

// plainError puts a problem the way the page shows it: never a path inside the box.
func plainError(err error) string {
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return errDiskFull.Error()
	}
	var pathError *fs.PathError
	if errors.As(err, &pathError) {
		return "Scan Box couldn't use its files on the box (" + pathError.Err.Error() + ")"
	}
	return err.Error()
}

// scanInto scans one page and hands it to keep, which adds it to the open document or
// finishes it as a document of its own.
func (s *Station) scanInto(ctx context.Context, device scanner.Device, mode scanner.Mode, keep func(scannedPage) error) scanner.Device {
	if device == nil {
		s.report(scanner.ErrNoScanner)
		return nil
	}
	current := s.config.Settings.Get()
	deadline := s.scanDeadline(current.ResolutionDPI)
	scanCtx, cancel := context.WithTimeout(ctx, deadline)
	done := s.watchScan(deadline)
	picture, err := device.Scan(scanCtx, requestFor(current, mode))
	done()
	cancel()
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		err = fmt.Errorf("the scan took longer than %v and was stopped: %w", deadline, err)
	}
	if err != nil {
		s.report(err)
		return s.lose(device, err)
	}
	s.setState(func() { s.saving = true })
	scanned, err := encodePage(picture, mode, current)
	if err == nil {
		err = keep(scanned)
	}
	// The page is stored, and may already be on its way to Paperless: saving ends before the
	// memory is handed back, not after.
	s.setState(func() { s.saving = false })
	picture = nil
	releasePage()
	s.report(err)
	return device
}

// releasePage hands the memory of the page just encoded back at once. A colour page at
// 600 dpi is a 139 MB picture; left to the collector's own pace, a few of them pile up as
// garbage before it runs. Collecting after each page costs milliseconds against seconds
// of scanning.
func releasePage() { runtime.GC() }

func (s *Station) keepOpenPage(scanned scannedPage) error {
	s.files.Lock()
	defer s.files.Unlock()
	_, err := s.disk.addOpenPage(scanned)
	return err
}

func (s *Station) finishSinglePage(scanned scannedPage) error {
	return s.finish(1, func(int) (scannedPage, error) { return scanned, nil }, false)
}

// finishOpenDocument makes a document of the open pages, reading each from its files only
// when the PDF needs it.
func (s *Station) finishOpenDocument() error {
	s.files.Lock()
	numbers, err := s.disk.openPageNumbers()
	s.files.Unlock()
	if err != nil {
		return err
	}
	if len(numbers) == 0 {
		return ErrNoPages
	}
	return s.finish(len(numbers), func(index int) (scannedPage, error) { return s.disk.readOpenPage(numbers[index]) }, true)
}

// finish writes a document of count pages, given one at a time by load, and clears the
// open document when the pages came from it.
func (s *Station) finish(count int, load func(int) (scannedPage, error), clearOpen bool) error {
	current := s.config.Settings.Get()
	now := s.config.Now()
	state := Waiting
	if current.ReviewBeforeSending {
		state = ToReview
	}
	location := current.Clock().Location
	s.files.Lock()
	document := Document{ID: s.disk.newID(now, location), Name: current.DocumentName(now), Created: now, State: state}
	write := s.disk.writeDocument
	if clearOpen {
		write = s.disk.turnOpenIntoDocument
	}
	err := write(document, count, load, location)
	s.files.Unlock()
	if err != nil {
		return err
	}
	if state == Waiting {
		s.SendWaiting()
	}
	return nil
}

func (s *Station) removeLastPage() error {
	s.files.Lock()
	defer s.files.Unlock()
	numbers, err := s.disk.openPageNumbers()
	if err != nil {
		return err
	}
	if len(numbers) == 0 {
		return ErrNoPages
	}
	return s.disk.removeOpenPage(numbers[len(numbers)-1])
}

func (s *Station) finishIfForgotten() {
	last, open := s.disk.lastOpenPageTime()
	idle := time.Duration(s.config.Settings.Get().IdleFinishMinutes) * time.Minute
	if !open || s.config.Now().Before(last.Add(idle)) {
		return
	}
	log.Printf("no new page for %v: finishing the open document", idle)
	s.report(s.finishOpenDocument())
}

// ---- finished documents ----

// Rename changes a scan's name. Before sending, it is the title Paperless gets; once
// sent, only Scan Box's copy changes, and the title Paperless got is kept as SentName.
func (s *Station) Rename(id, name string) error {
	if err := settings.CheckName(name); err != nil {
		return err
	}
	return s.changeDocument(id, func(document *Document) error {
		if document.State == Sent && document.SentName == "" {
			document.SentName = document.Name
		}
		document.Name = name
		log.Printf("scan %s: renamed", document.ID)
		return s.disk.save(*document)
	})
}

// Send marks a reviewed scan for upload and starts it.
func (s *Station) Send(id string) error {
	err := s.changeDocument(id, func(document *Document) error {
		if document.State != ToReview {
			return nil
		}
		log.Printf("scan %s: to be sent, as asked", document.ID)
		document.State = Waiting
		return s.disk.save(*document)
	})
	if err == nil {
		s.SendWaiting()
	}
	return err
}

// Delete removes a scan from Scan Box. A sent scan stays in Paperless. One being sent at
// this moment is refused: it would arrive in Paperless though the page said it was gone.
func (s *Station) Delete(id string) error {
	s.mutex.Lock()
	sending := s.sending == id
	s.mutex.Unlock()
	if sending {
		return ErrSending
	}
	return s.changeDocument(id, func(document *Document) error {
		log.Printf("scan %s: deleted, as asked, while %s", document.ID, document.State)
		return s.disk.remove(document.ID)
	})
}

func (s *Station) changeDocument(id string, change func(*Document) error) error {
	s.files.Lock()
	found, err := s.disk.find(id)
	if err == nil {
		err = change(&found.document)
	}
	s.files.Unlock()
	if err == nil {
		s.changed()
	}
	return err
}

// Document gives one scan's facts.
func (s *Station) Document(id string) (Document, error) {
	s.files.Lock()
	defer s.files.Unlock()
	found, err := s.disk.find(id)
	return found.document, err
}

func (s *Station) PDFPath(id string) (string, error) {
	s.files.Lock()
	defer s.files.Unlock()
	return s.disk.pdfPath(id)
}

func (s *Station) ThumbnailPath(id string, page int) (string, error) {
	s.files.Lock()
	defer s.files.Unlock()
	return s.disk.pagePicture(id, page, pageThumbFormat)
}

func (s *Station) PreviewPath(id string, page int) (string, error) {
	s.files.Lock()
	defer s.files.Unlock()
	return s.disk.previewPath(id, page)
}

func (s *Station) OpenPreviewPath(position int) (string, error) {
	s.files.Lock()
	defer s.files.Unlock()
	return s.disk.openPagePicture(position, pagePreviewFormat)
}

func (s *Station) OpenThumbnailPath(position int) (string, error) {
	s.files.Lock()
	defer s.files.Unlock()
	return s.disk.openPagePicture(position, pageThumbFormat)
}

// Usage says how much space the kept scans take, and how much is left.
// Damaged lists, from the data folder, the scan folders whose facts couldn't be read at
// the start, so they aren't listed.
func (s *Station) Damaged() []string {
	s.files.Lock()
	defer s.files.Unlock()
	return slices.Clone(s.disk.damaged)
}

func (s *Station) Usage() (used, free int64) {
	s.files.Lock()
	used = s.disk.usedBytes()
	s.files.Unlock()
	free, _ = s.config.FreeSpace(s.config.DataDir)
	return used, free
}

// ---- uploads ----

// SendWaiting starts an upload round now instead of at the next retry.
func (s *Station) SendWaiting() {
	select {
	case s.uploadNow <- struct{}{}:
	default:
	}
}

func (s *Station) runUploads(ctx context.Context) {
	retry := time.NewTicker(s.config.Retry)
	defer retry.Stop()
	for {
		s.beatUploads()
		s.uploadWaiting(ctx)
		s.confirmSubmitted(ctx)
		s.forgetOldSent()
		s.beatUploads()
		select {
		case <-ctx.Done():
			return
		case <-retry.C:
		case <-s.uploadNow:
		}
	}
}

// uploadWaiting sends the waiting documents, oldest first. One the destination refuses is
// set aside as failed, and one that came to nothing waits out its own pause; either way
// the round goes on. A refusal every document would meet stops the round and pauses
// uploads instead.
func (s *Station) uploadWaiting(ctx context.Context) {
	now := s.config.Now()
	s.mutex.Lock()
	paused := now.Before(s.pausedUntil)
	s.mutex.Unlock()
	if paused {
		return
	}
	s.files.Lock()
	waiting := s.disk.list(Waiting)
	s.files.Unlock()
	tried, problem := false, ""
	for _, document := range waiting {
		if ctx.Err() != nil {
			return
		}
		if now.Before(document.NextTry) {
			continue
		}
		err := s.upload(ctx, document)
		s.beatUploads()
		tried = true
		if errors.Is(err, ErrDenied) {
			s.pause(err)
			return
		}
		if err != nil && !errors.Is(err, errSettling) && !errors.Is(err, ErrRejected) {
			problem = plainError(err)
		}
	}
	if tried {
		s.setState(func() { s.uploadError, s.denials = problem, 0 })
	}
}

// pause stops uploads for a while after a refusal every document would meet. Each such
// round in a row doubles the pause.
func (s *Station) pause(denied error) {
	now := s.config.Now()
	s.setState(func() {
		s.denials++
		s.uploadError = denied.Error()
		s.pausedUntil = now.Add(s.pauseAfter(s.denials))
		log.Printf("uploads: paused until %s: %v", s.pausedUntil.Format(time.TimeOnly), denied)
	})
}

// pauseAfter is the pause after so many tries in a row that came to nothing: the retry
// interval, doubled with each one, up to longestPause.
func (s *Station) pauseAfter(tries int) time.Duration {
	pause := s.config.Retry
	for range tries - 1 {
		if pause >= longestPause/2 {
			return longestPause
		}
		pause *= 2
	}
	return min(pause, longestPause)
}

// Retry sends a failed scan again, or a waiting one at once.
func (s *Station) Retry(id string) error {
	err := s.changeDocument(id, func(document *Document) error {
		if document.State != Failed && document.State != Waiting {
			return nil
		}
		log.Printf("scan %s: to be sent again, as asked", document.ID)
		document.State, document.LastError = Waiting, ""
		document.Tries, document.NextTry = 0, time.Time{}
		return s.disk.save(*document)
	})
	if err == nil {
		s.SendWaiting()
	}
	return err
}

// RetryNow sends every waiting scan at once, whatever pause it was waiting out.
func (s *Station) RetryNow() {
	log.Print("uploads: sending every waiting scan now, without waiting out pauses")
	s.files.Lock()
	for _, document := range s.disk.list(Waiting) {
		if document.NextTry.IsZero() {
			continue
		}
		document.NextTry = time.Time{}
		s.disk.save(document)
	}
	s.files.Unlock()
	s.setState(func() { s.pausedUntil = time.Time{} })
	s.SendWaiting()
}

// upload sends one waiting document. The try is written down before it starts, so an
// upload whose outcome isn't known, even one a stop cut short, is looked for at the
// destination before the document is sent again.
func (s *Station) upload(ctx context.Context, document Document) error {
	if !document.AttemptedAt.IsZero() {
		resend, err := s.settleAttempt(ctx, document)
		if err != nil || !resend {
			return err
		}
	}
	s.files.Lock()
	path, err := s.disk.pdfPath(document.ID)
	if err != nil {
		s.files.Unlock()
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		log.Printf("upload %s: %v", document.ID, errNoPDF)
		document.State, document.LastError = Failed, errNoPDF.Error()
		err := s.disk.save(document)
		s.files.Unlock()
		return err
	}
	document.AttemptedAt, document.AttemptEnded = s.config.Now(), time.Time{}
	err = s.disk.save(document)
	s.files.Unlock()
	if err != nil {
		return err
	}
	s.sendingNow(document.ID)
	task, err := s.config.Destination.Upload(ctx, document.Name, document.UploadName(), path, document.Created)
	s.sendingNow("")
	return s.recordUpload(document.ID, document.Name, task, err)
}

// recordUpload writes down how the upload of a document under the title uploadedName went,
// on the document's record as it is now: a rename made meanwhile stays, and only the
// upload's own fields change. Accepted settles the try; rejected sets the document aside as
// failed; denied leaves it waiting, as the round stops; unclear keeps it marked as tried, to
// be looked for; anything else settles the try. An unclear upload and anything else make
// the document wait a pause that doubles each time.
func (s *Station) recordUpload(id, uploadedName, task string, uploadErr error) error {
	s.files.Lock()
	defer s.files.Unlock()
	current, err := s.disk.find(id)
	if err != nil || current.document.State != Waiting {
		return uploadErr
	}
	document := current.document
	now := s.config.Now()
	switch {
	case uploadErr == nil:
		log.Printf("upload %s: accepted, task %s", document.ID, task)
		document.State, document.Task, document.SubmittedAt, document.LastError = Submitted, task, now, ""
		document.SentName = uploadedName
		document.settleTry()
	case errors.Is(uploadErr, ErrRejected):
		log.Printf("upload %s: refused, set aside as failed: %v", document.ID, uploadErr)
		document.State, document.LastError = Failed, plainError(uploadErr)
		document.settleTry()
	case errors.Is(uploadErr, ErrDenied):
		log.Printf("upload %s: refused, as every upload would be: %v", document.ID, uploadErr)
		document.AttemptedAt, document.LastError = time.Time{}, plainError(uploadErr)
	case errors.Is(uploadErr, ErrUnclear):
		document.AttemptEnded, document.LastError = now, plainError(uploadErr)
		document.Tries++
		document.NextTry = now.Add(s.pauseAfter(document.Tries))
		log.Printf("upload %s: unclear, to be looked for before any resend: %v", document.ID, uploadErr)
	default:
		document.AttemptedAt, document.LastError = time.Time{}, plainError(uploadErr)
		document.Tries++
		document.NextTry = now.Add(s.pauseAfter(document.Tries))
		log.Printf("upload %s: failed, next try at %s: %v", document.ID, document.NextTry.Format(time.TimeOnly), uploadErr)
	}
	if err := s.disk.save(document); err != nil && uploadErr == nil {
		return err
	}
	return uploadErr
}

// settleAttempt finds out what became of an upload whose outcome wasn't known. If the
// destination has it, the document goes on as accepted. If not, it is sent again only
// once SettleWait has passed since that upload ended, since it may still turn up until
// then; meanwhile it says so, and the documents behind it go on.
func (s *Station) settleAttempt(ctx context.Context, document Document) (bool, error) {
	receipt, err := s.config.Destination.Find(ctx, document.UploadName())
	if err != nil {
		return false, err
	}
	now := s.config.Now()
	ended := document.AttemptEnded
	if ended.IsZero() {
		ended = s.started
	}
	if receipt == "" && now.Before(ended.Add(s.config.SettleWait)) {
		return false, errSettling
	}
	s.files.Lock()
	defer s.files.Unlock()
	current, err := s.disk.find(document.ID)
	if err != nil || current.document.State != Waiting {
		return false, nil
	}
	settled := current.document
	settled.AttemptedAt, settled.AttemptEnded = time.Time{}, time.Time{}
	if receipt == "" {
		log.Printf("upload %s: not found at the destination after the wait; sending again", settled.ID)
	}
	if receipt != "" {
		log.Printf("upload %s: found at the destination, task %s; not sent again", settled.ID, receipt)
		settled.State, settled.Task, settled.SubmittedAt, settled.LastError = Submitted, receipt, now, ""
		settled.SentName = settled.Name
		settled.settleTry()
	}
	if err := s.disk.save(settled); err != nil {
		return false, err
	}
	return receipt == "", nil
}

// confirmSubmitted asks the destination what became of each accepted document. A problem
// asking ends the round, as it would meet every other document too.
func (s *Station) confirmSubmitted(ctx context.Context) {
	s.files.Lock()
	submitted := s.disk.list(Submitted)
	s.files.Unlock()
	for _, document := range submitted {
		if ctx.Err() != nil {
			return
		}
		delivery, err := s.config.Destination.Confirm(ctx, document.Task)
		s.beatUploads()
		if err != nil {
			s.setState(func() {
				if s.confirmError == nil || s.confirmError.Error() != err.Error() {
					log.Printf("confirming: %v", err)
				}
				s.confirmError = err
			})
			return
		}
		s.settle(document.ID, delivery)
	}
	s.setState(func() { s.confirmError = nil })
}

// settle records what the destination said: stored, not stored, or nothing yet. An upload
// the destination still doesn't list after ConfirmWait counts as not stored. The page
// hears of it when the round ends.
func (s *Station) settle(id string, delivery Delivery) {
	now := s.config.Now()
	s.files.Lock()
	defer s.files.Unlock()
	found, err := s.disk.find(id)
	if err != nil || found.document.State != Submitted {
		return
	}
	document := found.document
	switch {
	case delivery.DocumentID != 0:
		document.State, document.DocumentID, document.SentAt, document.LastError = Sent, delivery.DocumentID, now, ""
		log.Printf("upload %s: stored as document %d", id, delivery.DocumentID)
	case delivery.Failure != "":
		document.State, document.LastError = Failed, delivery.Failure
		log.Printf("upload %s: not stored: %s", id, delivery.Failure)
	case !delivery.Listed && now.After(document.SubmittedAt.Add(s.config.ConfirmWait)):
		document.State, document.LastError = Failed, notListed
		log.Printf("upload %s: not stored: %s", id, notListed)
	default:
		return
	}
	s.disk.save(document)
}

// forgetOldSent removes sent scans whose storing was confirmed longer ago than the setting
// says; 0 keeps them always. Nothing that isn't confirmed as stored is ever removed here.
func (s *Station) forgetOldSent() {
	days := s.config.Settings.Get().KeepSentDays
	if days == 0 {
		return
	}
	cutoff := s.config.Now().Add(-time.Duration(days) * hoursPerDay * time.Hour)
	s.files.Lock()
	defer s.files.Unlock()
	for _, document := range s.disk.list(Sent) {
		if document.DocumentID != 0 && document.SentAt.Before(cutoff) {
			log.Printf("scan %s: removed, %d days after Paperless stored it as document %d", document.ID, days, document.DocumentID)
			s.disk.remove(document.ID)
		}
	}
}
