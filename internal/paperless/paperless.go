// Package paperless uploads finished documents to Paperless-ngx with an API token.
package paperless

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"scanbox/internal/settings"
	"scanbox/internal/station"
)

const (
	uploadPath = "/api/documents/post_document/"
	// probePath is a real API answer any signed-in user may read, and asking it changes
	// nothing. Paperless 3's /api/ itself only redirects to its HTML API viewer.
	probePath = "/api/statistics/"
	tasksPath = "/api/tasks/"
	// taskIDParameter picks one task out of Paperless's task list by its ID.
	taskIDParameter = "task_id"
	// nameParameter narrows the task list to uploads whose file name holds the text.
	nameParameter     = "name"
	orderingParameter = "ordering"
	oldestFirst       = "date_created"
	// tasksAccept asks for the task list as API version 10 shapes it: a paginated "results"
	// list, lowercase statuses, and what the consumption left in "result_data".
	tasksAccept    = "application/json; version=10"
	requestTimeout = 2 * time.Minute
	probeTimeout   = 10 * time.Second
	taskTimeout    = 15 * time.Second
	// stallTimeout is how long an upload may stand still before it is given up.
	stallTimeout = 2 * time.Minute
	// answerTimeout is how long Paperless may take to answer once all of an upload is sent.
	answerTimeout    = 2 * time.Minute
	answerLimitBytes = 4096
	// taskAnswerLimitBytes leaves room for a failed task's traceback, which Paperless keeps.
	taskAnswerLimitBytes = 64 * 1024
	documentField        = "document"
	titleField           = "title"
	// createdField takes the day Paperless files a document under. Given as the start of the
	// day with no zone, it is read in Paperless's own zone, which keeps the very day given
	// whatever zone Paperless runs in; Paperless keeps only the date.
	createdField = "created"
	startOfDay   = "T00:00:00"
	// Paperless adds these to every answer to a request it has signed in.
	versionHeader    = "X-Version"
	apiVersionHeader = "X-Api-Version"
	jsonMediaType    = "application/json"
	// excerptLength is how much of an unexpected answer a reason quotes.
	excerptLength = 60
	// detailLength is how much of Paperless's own explanation a reason quotes.
	detailLength  = 200
	noContentType = "no content type"
	// Paperless's task statuses, as API version 10 writes them; pending and started mean
	// the task isn't finished.
	taskSuccess = "success"
	taskFailure = "failure"
	taskRevoked = "revoked"
)

// taskID is how Paperless names the task it queues for an upload: a UUID.
var taskID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ServerInfo is what Paperless says about itself.
type ServerInfo struct {
	Version    string
	APIVersion string
}

var (
	ErrNoAddress   = errors.New("no Paperless address is set yet")
	ErrNoToken     = errors.New("no Paperless token is set in the container")
	ErrUnreachable = errors.New("Paperless can't be reached")
	// ErrUntrusted is an https address whose certificate no authority Scan Box trusts has
	// signed, such as a self-signed one or one from a home certificate authority.
	ErrUntrusted     = errors.New("Paperless's certificate isn't trusted")
	ErrTokenRejected = errors.New("Paperless refused the token")
	ErrNotAllowed    = errors.New("the token's user isn't allowed to add documents")
	// ErrNoReceipt is an answer to an upload that isn't Paperless's receipt for it, such as
	// a proxy's sign-in page: the upload can't be counted as received.
	ErrNoReceipt = errors.New("Paperless didn't confirm the upload; check the address")
	// ErrRedirected is an answer that points elsewhere. It isn't followed, so the token
	// goes nowhere but the address that is set.
	ErrRedirected = errors.New("Paperless redirected the request")
	// ErrNoTaskAccess is a token whose user may add documents but not see the tasks that
	// store them, so Scan Box can't tell whether a document was stored.
	ErrNoTaskAccess = errors.New("the token's user may not view tasks, so Scan Box can't confirm that scans were stored. " +
		"In Paperless, open Users & Groups, edit that user and tick View for PaperlessTask")
	// ErrTaskVersion is a Paperless that doesn't offer the task list in API version 10.
	ErrTaskVersion = errors.New("Paperless doesn't offer its task list in API version 10, so uploads can't be confirmed; a newer Paperless is needed")
	// ErrTaskAnswer is a task list Scan Box can't read.
	ErrTaskAnswer = errors.New("Paperless's task list couldn't be read")

	errStalled  = errors.New("the upload stood still")
	errNoAnswer = errors.New("Paperless didn't answer")
)

// taskList is the part of Paperless's task list that says what became of an upload.
type taskList struct {
	Results []listedTask `json:"results"`
}

type listedTask struct {
	TaskID    string `json:"task_id"`
	Status    string `json:"status"`
	InputData struct {
		Filename string `json:"filename"`
	} `json:"input_data"`
	ResultData json.RawMessage `json:"result_data"`
}

// taskResult is what a finished consumption leaves in result_data: the document it
// made, the document it duplicates, or why it stopped.
type taskResult struct {
	DocumentID       int    `json:"document_id"`
	DuplicateOf      int    `json:"duplicate_of"`
	DuplicateInTrash bool   `json:"duplicate_in_trash"`
	ErrorMessage     string `json:"error_message"`
	Reason           string `json:"reason"`
}

// Client talks to one Paperless address with one token.
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
	// stall and answerWait replace stallTimeout and answerTimeout when set.
	stall      time.Duration
	answerWait time.Duration
}

// Upload sends one PDF under the given file name and title, and returns the task Paperless
// queued for it. Unless created is zero, Paperless files the document under created's date
// in created's zone; otherwise Paperless dates it itself.
func (c Client) Upload(ctx context.Context, title, fileName, path string, created time.Time) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	body, length, contentType, err := documentForm(title, fileName, created, file, info.Size())
	if err != nil {
		return "", err
	}
	answer, err := c.sendDocument(ctx, body, length, contentType)
	if refusesEverything(err) {
		return "", fmt.Errorf("%w; %w", err, station.ErrDenied)
	}
	if err != nil {
		return "", err
	}
	switch {
	case answer.status >= http.StatusInternalServerError:
		return "", fmt.Errorf("%w: %w", station.ErrUnclear, answer.unexpected())
	case rejectedStatuses[answer.status]:
		return "", fmt.Errorf("Paperless refused the file (%d %s: “%s”); %w", answer.status, http.StatusText(answer.status), answer.excerpt(), station.ErrRejected)
	case answer.status == http.StatusNotFound:
		return "", fmt.Errorf("%w; check the address; %w", answer.unexpected(), station.ErrDenied)
	case answer.status != http.StatusOK:
		return "", answer.unexpected()
	}
	task, err := answer.receipt()
	if err != nil {
		return "", fmt.Errorf("%w; %w", err, station.ErrDenied)
	}
	return task, nil
}

// rejectedStatuses are Paperless's answers that refuse this file as it is: invalid, too
// large or of a type it doesn't take. Sending it again unchanged would meet the same.
var rejectedStatuses = map[int]bool{
	http.StatusBadRequest:            true,
	http.StatusRequestEntityTooLarge: true,
	http.StatusUnsupportedMediaType:  true,
	http.StatusUnprocessableEntity:   true,
}

// refusesEverything says whether an upload met a refusal every upload would meet: the
// token, its permission to add documents, or an address that sends requests elsewhere.
func refusesEverything(err error) bool {
	return errors.Is(err, ErrTokenRejected) || errors.Is(err, ErrNotAllowed) || errors.Is(err, ErrRedirected)
}

// sendDocument sends an upload of length bytes with no overall time limit, so a large
// document on a slow link isn't cut off while it is still moving. It is given up when it
// stands still for the stall time, or when Paperless takes longer than the answer wait once
// all of it is sent. A failure after the connection was made is unclear: the document may
// have arrived.
func (c Client) sendDocument(ctx context.Context, body io.Reader, length int64, contentType string) (answer, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	stall, answerWait := orDefault(c.stall, stallTimeout), orDefault(c.answerWait, answerTimeout)
	var answering atomic.Bool
	watchdog := time.AfterFunc(stall, func() {
		if answering.Load() {
			cancel(fmt.Errorf("%w within %v", errNoAnswer, answerWait))
			return
		}
		cancel(fmt.Errorf("%w for %v", errStalled, stall))
	})
	defer watchdog.Stop()
	var connected atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }})
	moving := &progress{
		reader: body,
		moved:  func() { watchdog.Reset(stall) },
		finished: func() {
			answering.Store(true)
			watchdog.Reset(answerWait)
		},
	}
	request, err := c.request(ctx, http.MethodPost, uploadPath, moving)
	if err != nil {
		return answer{}, err
	}
	request.ContentLength = length
	request.Header.Set("Content-Type", contentType)
	sent, err := c.exchange(request, answerLimitBytes, ErrNotAllowed)
	if !errors.Is(err, ErrUnreachable) || !connected.Load() {
		return sent, err
	}
	if cause := context.Cause(ctx); errors.Is(cause, errStalled) || errors.Is(cause, errNoAnswer) {
		return answer{}, fmt.Errorf("%w: %w", station.ErrUnclear, cause)
	}
	return answer{}, fmt.Errorf("%w: %w", station.ErrUnclear, err)
}

// progress tells, as a request body is read, that it moved, and when all of it was read.
type progress struct {
	reader   io.Reader
	moved    func()
	finished func()
}

func (p *progress) Read(buffer []byte) (int, error) {
	count, err := p.reader.Read(buffer)
	if count > 0 {
		p.moved()
	}
	if errors.Is(err, io.EOF) {
		p.finished()
	}
	return count, err
}

func orDefault(value, fallback time.Duration) time.Duration {
	if value == 0 {
		return fallback
	}
	return value
}

// Confirm asks Paperless's task list what became of an upload, from the task ID its
// receipt gave. A task that is pending or started, or not listed, isn't settled yet.
func (c Client) Confirm(ctx context.Context, task string) (station.Delivery, error) {
	list, err := c.tasks(ctx, url.Values{taskIDParameter: {task}})
	if err != nil {
		return station.Delivery{}, err
	}
	for _, listed := range list.Results {
		if strings.EqualFold(listed.TaskID, task) {
			return listed.delivery(), nil
		}
	}
	return station.Delivery{}, nil
}

// Find gives the task of the first upload of a file with this name, or nothing if
// Paperless never queued one. Paperless lists an upload's task from the moment it queues
// it, before it answers, so the task is there whenever the upload arrived. The name
// filter matches part of a name, so only a whole match counts.
func (c Client) Find(ctx context.Context, fileName string) (string, error) {
	list, err := c.tasks(ctx, url.Values{nameParameter: {fileName}, orderingParameter: {oldestFirst}})
	if err != nil {
		return "", err
	}
	for _, listed := range list.Results {
		if listed.InputData.Filename == fileName {
			return listed.TaskID, nil
		}
	}
	return "", nil
}

// tasks reads Paperless's task list, narrowed by the query.
func (c Client) tasks(ctx context.Context, query url.Values) (taskList, error) {
	ctx, cancel := context.WithTimeout(ctx, taskTimeout)
	defer cancel()
	request, err := c.request(ctx, http.MethodGet, tasksPath+"?"+query.Encode(), nil)
	if err != nil {
		return taskList{}, err
	}
	request.Header.Set("Accept", tasksAccept)
	answer, err := c.exchange(request, taskAnswerLimitBytes, ErrNoTaskAccess)
	if err != nil {
		return taskList{}, err
	}
	if answer.status == http.StatusNotAcceptable {
		return taskList{}, ErrTaskVersion
	}
	if answer.status != http.StatusOK {
		return taskList{}, answer.unexpected()
	}
	var list taskList
	if answer.mediaType() != jsonMediaType || json.Unmarshal(answer.body, &list) != nil {
		return taskList{}, fmt.Errorf("%w (%s): %q", ErrTaskAnswer, answer.describedType(), answer.excerpt())
	}
	return list, nil
}

// delivery reads a listed task: stored as a document, not stored and why, or not finished.
func (t listedTask) delivery() station.Delivery {
	var result taskResult
	// A result that isn't an object, or no result yet, leaves it empty.
	json.Unmarshal(t.ResultData, &result)
	delivery := station.Delivery{Listed: true}
	switch t.Status {
	case taskSuccess:
		delivery.DocumentID = result.DocumentID
		if result.DocumentID == 0 {
			delivery.Failure = withDetail("Paperless finished without storing it as a document", result.Reason)
		}
	case taskFailure:
		delivery.Failure = result.failure()
	case taskRevoked:
		delivery.Failure = "Paperless cancelled the task before storing it"
	}
	return delivery
}

func (r taskResult) failure() string {
	if r.DuplicateOf == 0 {
		return withDetail("Paperless couldn't store it", r.ErrorMessage)
	}
	where := ""
	if r.DuplicateInTrash {
		where = ", in its trash"
	}
	return fmt.Sprintf("Paperless refused it as a copy of its document %d%s", r.DuplicateOf, where)
}

// withDetail adds Paperless's own explanation to a sentence, on one line and kept short.
func withDetail(sentence, detail string) string {
	if strings.TrimSpace(detail) == "" {
		return sentence
	}
	return sentence + " (" + shorten(detail, detailLength) + ")"
}

// receipt reads the task ID out of Paperless's answer to an upload: a JSON string holding
// a UUID. Anything else means the upload can't be counted as received.
func (a answer) receipt() (string, error) {
	var task string
	if a.mediaType() != jsonMediaType || json.Unmarshal(a.body, &task) != nil || !taskID.MatchString(task) {
		return "", fmt.Errorf("%w (status %d, %s): %q", ErrNoReceipt, a.status, a.describedType(), a.excerpt())
	}
	return task, nil
}

// Check proves the address and token work without adding anything: an upload without a
// document is refused as incomplete (400) only after the token and permission are accepted.
func (c Client) Check(ctx context.Context) error {
	body, contentType, err := emptyForm()
	if err != nil {
		return err
	}
	answer, err := c.post(ctx, body, contentType)
	if err != nil {
		return err
	}
	if answer.status == http.StatusBadRequest && strings.Contains(answer.text, documentField) {
		return nil
	}
	return answer.unexpected()
}

// Probe asks Paperless whether it answers and accepts the token, and reads the versions
// it reports, without changing anything. A redirect is reported, not followed.
func (c Client) Probe(ctx context.Context) (ServerInfo, error) {
	if c.BaseURL == "" {
		return ServerInfo{}, ErrNoAddress
	}
	if c.Token == "" {
		return ServerInfo{}, ErrNoToken
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+probePath, nil)
	if err != nil {
		return ServerInfo{}, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	request.Header.Set("Authorization", "Token "+c.Token)
	request.Header.Set("Accept", jsonMediaType)
	response, err := c.httpClient().Do(request)
	if err != nil {
		return ServerInfo{}, notReached(err)
	}
	defer response.Body.Close()
	io.Copy(io.Discard, io.LimitReader(response.Body, answerLimitBytes))
	if response.StatusCode == http.StatusUnauthorized {
		return ServerInfo{}, ErrTokenRejected
	}
	if response.StatusCode != http.StatusOK {
		return ServerInfo{}, fmt.Errorf("Paperless answered %d", response.StatusCode)
	}
	return ServerInfo{Version: response.Header.Get(versionHeader), APIVersion: response.Header.Get(apiVersionHeader)}, nil
}

type answer struct {
	status      int
	contentType string
	body        []byte
	text        string
}

func (a answer) mediaType() string {
	mediaType, _, err := mime.ParseMediaType(a.contentType)
	if err != nil {
		return ""
	}
	return mediaType
}

func (a answer) describedType() string {
	if a.contentType == "" {
		return noContentType
	}
	return a.contentType
}

// excerpt is the start of the answer on one line, enough to recognise a sign-in page or an
// error page without filling the log.
func (a answer) excerpt() string { return shorten(a.text, excerptLength) }

// shorten puts text on one line and cuts it to at most limit characters.
func shorten(text string, limit int) string {
	line := strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(line) <= limit {
		return line
	}
	return string([]rune(line)[:limit]) + "…"
}

// unexpected describes an answer that is neither a receipt nor what a check expects.
func (a answer) unexpected() error {
	return fmt.Errorf("Paperless answered %d (%s): %q", a.status, a.describedType(), a.excerpt())
}

// post sends a form to the upload address, bounded by requestTimeout: for the check, which
// carries no document.
func (c Client) post(ctx context.Context, body *bytes.Buffer, contentType string) (answer, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	request, err := c.request(ctx, http.MethodPost, uploadPath, body)
	if err != nil {
		return answer{}, err
	}
	request.Header.Set("Content-Type", contentType)
	return c.exchange(request, answerLimitBytes, ErrNotAllowed)
}

// request starts a request to Paperless that carries the token.
func (c Client) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	if c.BaseURL == "" {
		return nil, ErrNoAddress
	}
	if c.Token == "" {
		return nil, ErrNoToken
	}
	request, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	request.Header.Set("Authorization", "Token "+c.Token)
	return request, nil
}

// exchange makes the request and reads up to limit bytes of the answer. A refused token
// and a redirect come back as errors, and so does a 403, as forbidden: what the token's
// user lacks for this request.
func (c Client) exchange(request *http.Request, limit int64, forbidden error) (answer, error) {
	response, err := c.httpClient().Do(request)
	if err != nil {
		return answer{}, notReached(err)
	}
	defer response.Body.Close()
	content, _ := io.ReadAll(io.LimitReader(response.Body, limit))
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return answer{}, ErrTokenRejected
	case http.StatusForbidden:
		return answer{}, forbidden
	}
	if isRedirect(response.StatusCode) {
		return answer{}, fmt.Errorf("%w to %s; check the address", ErrRedirected, redirectTarget(response))
	}
	return answer{status: response.StatusCode, contentType: response.Header.Get("Content-Type"), body: content, text: strings.TrimSpace(string(content))}, nil
}

func isRedirect(status int) bool {
	return status >= http.StatusMultipleChoices && status < http.StatusBadRequest
}

// redirectTarget says where a redirect points, without its query: enough to see that it
// is a sign-in page, without repeating whatever the query carries.
func redirectTarget(response *http.Response) string {
	target, err := response.Location()
	if err != nil {
		return "an address it didn't give"
	}
	target.User, target.RawQuery, target.Fragment = nil, "", ""
	return target.String()
}

// httpClient never follows a redirect: a redirect would carry the token to wherever it
// points, and turn a sign-in page into an answer that looks like success.
// notReached says why a request got no answer: a certificate no trusted authority signed,
// or anything else on the way.
func notReached(err error) error {
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return fmt.Errorf("%w: %v", ErrUntrusted, err)
	}
	return fmt.Errorf("%w: %v", ErrUnreachable, err)
}

func (c Client) httpClient() *http.Client {
	client := http.Client{}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client
}

// emptyForm is the upload form with nothing in it, which the connection check sends.
func emptyForm() (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return &body, writer.FormDataContentType(), nil
}

// documentForm is the upload form with the title, the date unless created is zero, and the
// document, whose size bytes are read from file as they are sent, never held in memory. The
// form's head, written ahead, goes before the document, and its closing boundary after, so
// the length is known.
func documentForm(title, fileName string, created time.Time, file io.Reader, size int64) (io.Reader, int64, string, error) {
	var written bytes.Buffer
	writer := multipart.NewWriter(&written)
	if err := writer.WriteField(titleField, title); err != nil {
		return nil, 0, "", err
	}
	if err := writeCreated(writer, created); err != nil {
		return nil, 0, "", err
	}
	if _, err := writer.CreateFormFile(documentField, fileName); err != nil {
		return nil, 0, "", err
	}
	head := bytes.Clone(written.Bytes())
	written.Reset()
	if err := writer.Close(); err != nil {
		return nil, 0, "", err
	}
	tail := written.Bytes()
	body := io.MultiReader(bytes.NewReader(head), io.LimitReader(file, size), bytes.NewReader(tail))
	return body, int64(len(head)) + size + int64(len(tail)), writer.FormDataContentType(), nil
}

func writeCreated(writer *multipart.Writer, created time.Time) error {
	if created.IsZero() {
		return nil
	}
	return writer.WriteField(createdField, created.Format(time.DateOnly)+startOfDay)
}

// Destination uploads to whatever address the settings hold right now.
type Destination struct {
	Settings *settings.Store
	Token    string
}

func (d Destination) client() Client {
	return Client{BaseURL: d.Settings.Get().PaperlessURL, Token: d.Token}
}

// Upload dates the document by the day it was scanned when the settings ask for it.
func (d Destination) Upload(ctx context.Context, title, fileName, path string, scanned time.Time) (string, error) {
	current := d.Settings.Get()
	client := Client{BaseURL: current.PaperlessURL, Token: d.Token}
	if !current.SendScanDate {
		return client.Upload(ctx, title, fileName, path, time.Time{})
	}
	return client.Upload(ctx, title, fileName, path, scanned.In(current.Clock().Location))
}

func (d Destination) Confirm(ctx context.Context, task string) (station.Delivery, error) {
	return d.client().Confirm(ctx, task)
}

func (d Destination) Find(ctx context.Context, fileName string) (string, error) {
	return d.client().Find(ctx, fileName)
}

func (d Destination) Check(ctx context.Context) error {
	return d.client().Check(ctx)
}

func (d Destination) Probe(ctx context.Context) (ServerInfo, error) {
	return d.client().Probe(ctx)
}
