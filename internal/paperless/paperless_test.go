package paperless

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"scanbox/internal/settings"
	"scanbox/internal/station"
)

const (
	testToken = "test-token"
	// testTask is shaped like the task IDs Paperless gives: a UUID.
	testTask    = "6d1c5a1e-8f0b-4c3e-9a51-2f7d84b0c913"
	signInPage  = "<!doctype html>\n<title>Sign in</title>\n<form method=post></form>"
	loginPath   = "/accounts/login/"
	jsonContent = "application/json"
	// maxReasonLength bounds a reason: the sentence, the status, the content type and the excerpt.
	maxReasonLength = 300
)

type received struct {
	authorization string
	title         string
	fileName      string
	content       string
	// created holds the created fields the form had: none, or the one date sent.
	created []string
}

func fakePaperless(t *testing.T, last *received) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != uploadPath || request.Method != http.MethodPost {
			http.NotFound(writer, request)
			return
		}
		last.authorization = request.Header.Get("Authorization")
		if last.authorization != "Token "+testToken {
			http.Error(writer, `{"detail":"Invalid token."}`, http.StatusUnauthorized)
			return
		}
		if err := request.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		file, header, err := request.FormFile(documentField)
		if err != nil {
			http.Error(writer, `{"document":["No file was submitted."]}`, http.StatusBadRequest)
			return
		}
		content, _ := io.ReadAll(file)
		last.title, last.fileName, last.content = request.FormValue(titleField), header.Filename, string(content)
		last.created = request.MultipartForm.Value[createdField]
		writer.Header().Set("Content-Type", jsonContent)
		writer.Write([]byte(`"` + testTask + `"`))
	}))
}

func samplePDF(t *testing.T) string {
	path := filepath.Join(t.TempDir(), "Scan 2026-11-11 19-13-52.pdf")
	if err := os.WriteFile(path, []byte("%PDF-1.4 sample"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestUploadSendsTokenTitleAndFile(t *testing.T) {
	var last received
	server := fakePaperless(t, &last)
	defer server.Close()
	client := Client{BaseURL: server.URL, Token: testToken}
	task, err := client.Upload(context.Background(), "Gas bill November", "scanbox-20261111-191352.pdf", samplePDF(t), time.Time{})
	if err != nil || task != testTask {
		t.Fatalf("Upload = %q, %v", task, err)
	}
	if last.title != "Gas bill November" || last.fileName != "scanbox-20261111-191352.pdf" || last.content != "%PDF-1.4 sample" {
		t.Fatalf("received %+v", last)
	}
	if last.created != nil {
		t.Fatalf("no date was asked for, yet the form had created %q", last.created)
	}
}

// A scan finished at 00:30 in Bucharest is still 11/11 in UTC, where Paperless runs unless
// told otherwise; the date sent is the box's own day, with no zone for Paperless to shift.
func TestUploadDatesDocumentsByTheirScanDayWhenAsked(t *testing.T) {
	var last received
	server := fakePaperless(t, &last)
	defer server.Close()
	store, err := settings.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for field, value := range map[string]string{"paperless_url": server.URL, "time_zone": "Europe/Bucharest"} {
		if _, err := store.Set(field, value); err != nil {
			t.Fatal(err)
		}
	}
	destination := Destination{Settings: store, Token: testToken}
	scanned := time.Date(2026, 11, 11, 22, 30, 0, 0, time.UTC)
	if _, err := destination.Upload(context.Background(), "t", "scanbox-1.pdf", samplePDF(t), scanned); err != nil {
		t.Fatal(err)
	}
	if last.created != nil {
		t.Fatalf("left off, Paperless should date the document itself; the form had created %q", last.created)
	}
	if _, err := store.Set("send_scan_date", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := destination.Upload(context.Background(), "t", "scanbox-1.pdf", samplePDF(t), scanned); err != nil {
		t.Fatal(err)
	}
	if len(last.created) != 1 || last.created[0] != "2026-11-12T00:00:00" {
		t.Fatalf("created = %q, want the scan day in Bucharest, 2026-11-12T00:00:00", last.created)
	}
}

// A Paperless with a certificate of its own is named as such, not as one that can't be
// reached, and works once its authority is trusted.
func TestAnUntrustedCertificateIsNamed(t *testing.T) {
	server := httptest.NewTLSServer(answering(http.StatusOK, jsonContent, "{}"))
	defer server.Close()
	_, err := Client{BaseURL: server.URL, Token: testToken}.Probe(context.Background())
	if !errors.Is(err, ErrUntrusted) || errors.Is(err, ErrUnreachable) {
		t.Fatalf("a certificate no trusted authority signed: %v, want ErrUntrusted", err)
	}
	if _, err := (Client{BaseURL: server.URL, Token: testToken, HTTP: server.Client()}).Probe(context.Background()); err != nil {
		t.Fatalf("with its authority trusted: %v", err)
	}
}

// answering is a Paperless address that answers every upload the same way.
func answering(status int, contentType, body string) http.HandlerFunc {
	return func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", contentType)
		writer.WriteHeader(status)
		writer.Write([]byte(body))
	}
}

func TestOnlyPaperlessReceiptCountsAsSent(t *testing.T) {
	receipts := map[string]http.HandlerFunc{
		"a task ID as a JSON string":   answering(http.StatusOK, jsonContent, `"`+testTask+`"`),
		"the same, with a charset":     answering(http.StatusOK, jsonContent+"; charset=utf-8", `"`+testTask+`"`),
		"a task ID in capital letters": answering(http.StatusOK, jsonContent, `"`+strings.ToUpper(testTask)+`"`),
	}
	for name, handler := range receipts {
		server := httptest.NewServer(handler)
		task, err := Client{BaseURL: server.URL, Token: testToken}.Upload(context.Background(), "t", "scanbox-1.pdf", samplePDF(t), time.Time{})
		server.Close()
		if err != nil || !strings.EqualFold(task, testTask) {
			t.Errorf("%s: Upload = %q, %v", name, task, err)
		}
	}
	notReceipts := map[string]http.HandlerFunc{
		"a sign-in page":             answering(http.StatusOK, "text/html; charset=utf-8", signInPage),
		"no content type":            answering(http.StatusOK, "", `"`+testTask+`"`),
		"a task ID as plain text":    answering(http.StatusOK, "text/plain", `"`+testTask+`"`),
		"malformed JSON":             answering(http.StatusOK, jsonContent, `{"task_id":`),
		"a JSON string, not a UUID":  answering(http.StatusOK, jsonContent, `"queued"`),
		"a JSON object":              answering(http.StatusOK, jsonContent, `{"task_id":"`+testTask+`"}`),
		"a UUID with something more": answering(http.StatusOK, jsonContent, `"`+testTask+`x"`),
	}
	for name, handler := range notReceipts {
		server := httptest.NewServer(handler)
		task, err := Client{BaseURL: server.URL, Token: testToken}.Upload(context.Background(), "t", "scanbox-1.pdf", samplePDF(t), time.Time{})
		server.Close()
		if !errors.Is(err, ErrNoReceipt) || task != "" {
			t.Errorf("%s: Upload = %q, %v", name, task, err)
			continue
		}
		if !strings.Contains(err.Error(), "status 200") || strings.ContainsAny(err.Error(), "\n\r") {
			t.Errorf("%s: the reason should name the status, on one line: %q", name, err)
		}
	}
	server := httptest.NewServer(answering(http.StatusOK, "text/html; charset=utf-8", signInPage+strings.Repeat(" padding", 100)))
	defer server.Close()
	_, err := Client{BaseURL: server.URL, Token: testToken}.Upload(context.Background(), "t", "scanbox-1.pdf", samplePDF(t), time.Time{})
	if err == nil || !strings.Contains(err.Error(), "text/html") || !strings.Contains(err.Error(), "Sign in") || len(err.Error()) > maxReasonLength {
		t.Fatalf("the reason should give the content type and a short excerpt: %q", err)
	}
}

func TestRedirectsAreReportedNotFollowed(t *testing.T) {
	followed := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == loginPath {
			followed = true
			answering(http.StatusOK, "text/html", signInPage)(writer, request)
			return
		}
		http.Redirect(writer, request, loginPath+"?next="+request.URL.Path, http.StatusFound)
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, Token: testToken}
	_, uploadErr := client.Upload(context.Background(), "t", "scanbox-1.pdf", samplePDF(t), time.Time{})
	checkErr := client.Check(context.Background())
	for name, err := range map[string]error{"Upload": uploadErr, "Check": checkErr} {
		if !errors.Is(err, ErrRedirected) || !strings.Contains(err.Error(), server.URL+loginPath) || strings.Contains(err.Error(), "next=") {
			t.Errorf("%s after a redirect: %v", name, err)
		}
	}
	if followed {
		t.Fatal("a redirect was followed, and the token sent on")
	}
}

// taskServer lists tasks the way Paperless 3 does in API version 10, and records what was asked.
func taskServer(t *testing.T, listed string, asked *url.Values) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != tasksPath || request.Method != http.MethodGet {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Token "+testToken {
			http.Error(writer, `{"detail":"Invalid token."}`, http.StatusUnauthorized)
			return
		}
		if request.Header.Get("Accept") != tasksAccept {
			http.Error(writer, `{"detail":"Invalid version in \"Accept\" header."}`, http.StatusNotAcceptable)
			return
		}
		*asked = request.URL.Query()
		writer.Header().Set("Content-Type", jsonContent)
		fmt.Fprintf(writer, `{"count":1,"next":null,"previous":null,"all":[1],"results":[%s]}`, listed)
	}))
}

func TestConfirmReadsWhatBecameOfTheUpload(t *testing.T) {
	other := `{"task_id":"11111111-2222-3333-4444-555555555555","status":"success","result_data":{"document_id":7}},`
	cases := []struct {
		name   string
		listed string
		want   station.Delivery
		reason string
	}{
		{"stored", `{"task_id":"` + testTask + `","status":"success","result_data":{"document_id":1234}}`, station.Delivery{DocumentID: 1234, Listed: true}, ""},
		{"another task listed first", other + `{"task_id":"` + testTask + `","status":"success","result_data":{"document_id":99}}`, station.Delivery{DocumentID: 99, Listed: true}, ""},
		{"waiting in the queue", `{"task_id":"` + testTask + `","status":"pending","result_data":null}`, station.Delivery{Listed: true}, ""},
		{"being consumed", `{"task_id":"` + testTask + `","status":"started","result_data":null}`, station.Delivery{Listed: true}, ""},
		{"failed", `{"task_id":"` + testTask + `","status":"failure","result_data":{"error_type":"ConsumerError","error_message":"Unsupported mime type\napplication/x-test"}}`, station.Delivery{Listed: true}, "Paperless couldn't store it (Unsupported mime type application/x-test)"},
		{"a duplicate", `{"task_id":"` + testTask + `","status":"failure","result_data":{"duplicate_of":321,"duplicate_in_trash":false}}`, station.Delivery{Listed: true}, "Paperless refused it as a copy of its document 321"},
		{"a duplicate in the trash", `{"task_id":"` + testTask + `","status":"failure","result_data":{"duplicate_of":5,"duplicate_in_trash":true}}`, station.Delivery{Listed: true}, "Paperless refused it as a copy of its document 5, in its trash"},
		{"cancelled", `{"task_id":"` + testTask + `","status":"revoked","result_data":null}`, station.Delivery{Listed: true}, "Paperless cancelled the task before storing it"},
		{"finished without a document", `{"task_id":"` + testTask + `","status":"success","result_data":{"reason":"Barcode splitting complete!"}}`, station.Delivery{Listed: true}, "Paperless finished without storing it as a document (Barcode splitting complete!)"},
		{"failed without a reason", `{"task_id":"` + testTask + `","status":"failure","result_data":"odd"}`, station.Delivery{Listed: true}, "Paperless couldn't store it"},
		{"not listed", strings.TrimSuffix(other, ","), station.Delivery{}, ""},
	}
	for _, example := range cases {
		var asked url.Values
		server := taskServer(t, example.listed, &asked)
		delivery, err := Client{BaseURL: server.URL, Token: testToken}.Confirm(context.Background(), testTask)
		server.Close()
		example.want.Failure = example.reason
		if err != nil || delivery != example.want {
			t.Errorf("%s: Confirm = %+v, %v; want %+v", example.name, delivery, err, example.want)
		}
		if asked.Get(taskIDParameter) != testTask {
			t.Errorf("%s: asked for %v", example.name, asked)
		}
	}
}

func TestConfirmSaysWhatIsMissing(t *testing.T) {
	cases := map[string]struct {
		handler http.HandlerFunc
		want    error
	}{
		"no permission to view tasks":    {answering(http.StatusForbidden, jsonContent, `{"detail":"You do not have permission to perform this action."}`), ErrNoTaskAccess},
		"a Paperless without version 10": {answering(http.StatusNotAcceptable, jsonContent, `{"detail":"Invalid version"}`), ErrTaskVersion},
		"a refused token":                {answering(http.StatusUnauthorized, jsonContent, `{"detail":"Invalid token."}`), ErrTokenRejected},
		"a sign-in page":                 {answering(http.StatusOK, "text/html", signInPage), ErrTaskAnswer},
		"a redirect": {func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, loginPath, http.StatusFound)
		}, ErrRedirected},
	}
	for name, example := range cases {
		server := httptest.NewServer(example.handler)
		_, err := Client{BaseURL: server.URL, Token: testToken}.Confirm(context.Background(), testTask)
		server.Close()
		if !errors.Is(err, example.want) {
			t.Errorf("%s: Confirm gave %v, want %v", name, err, example.want)
		}
	}
	if !strings.Contains(ErrNoTaskAccess.Error(), "PaperlessTask") {
		t.Fatal("the missing permission should be named")
	}
}

func TestFindGivesTheFirstUploadOfTheFile(t *testing.T) {
	const fileName = "scanbox-12-11-2026_16-51-27.pdf"
	listed := `{"task_id":"aaaaaaaa-0000-4000-8000-000000000001","status":"success","input_data":{"filename":"x` + fileName + `"}},` +
		`{"task_id":"` + testTask + `","status":"started","input_data":{"filename":"` + fileName + `"}},` +
		`{"task_id":"bbbbbbbb-0000-4000-8000-000000000002","status":"success","input_data":{"filename":"` + fileName + `"}}`
	var asked url.Values
	server := taskServer(t, listed, &asked)
	defer server.Close()
	client := Client{BaseURL: server.URL, Token: testToken}
	task, err := client.Find(context.Background(), fileName)
	if err != nil || task != testTask {
		t.Fatalf("Find = %q, %v", task, err)
	}
	if asked.Get(nameParameter) != fileName || asked.Get(orderingParameter) != oldestFirst {
		t.Fatalf("asked for %v", asked)
	}
	if task, err := client.Find(context.Background(), "scanbox-01-01-2026_00-00-00.pdf"); err != nil || task != "" {
		t.Fatalf("a file never uploaded: %q, %v", task, err)
	}
}

// largePDF is a document too big to vanish into the connection's buffers.
func largePDF(t *testing.T, megabytes int) string {
	path := filepath.Join(t.TempDir(), "large.pdf")
	if err := os.WriteFile(path, make([]byte, megabytes<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAnUploadThatBreaksOffIsUnclear(t *testing.T) {
	quick := func(client Client) Client {
		client.stall, client.answerWait = 200*time.Millisecond, 200*time.Millisecond
		return client
	}
	cases := map[string]struct {
		handler http.HandlerFunc
		pdf     string
		cause   error
	}{
		"the connection closes after the upload": {func(writer http.ResponseWriter, request *http.Request) {
			io.Copy(io.Discard, request.Body)
			connection, _, _ := writer.(http.Hijacker).Hijack()
			connection.Close()
		}, samplePDF(t), nil},
		"Paperless's own fault": {answering(http.StatusBadGateway, "text/html", "<title>502 Bad Gateway</title>"), samplePDF(t), nil},
		"the upload stands still": {func(writer http.ResponseWriter, request *http.Request) {
			io.CopyN(io.Discard, request.Body, 1<<20)
			time.Sleep(2 * time.Second)
		}, largePDF(t, 64), errStalled},
		"the answer comes too late": {func(writer http.ResponseWriter, request *http.Request) {
			io.Copy(io.Discard, request.Body)
			time.Sleep(time.Second)
			answering(http.StatusOK, jsonContent, `"`+testTask+`"`)(writer, request)
		}, samplePDF(t), errNoAnswer},
	}
	for name, example := range cases {
		server := httptest.NewServer(example.handler)
		started := time.Now()
		task, err := quick(Client{BaseURL: server.URL, Token: testToken}).Upload(context.Background(), "t", "scanbox-1.pdf", example.pdf, time.Time{})
		took := time.Since(started)
		server.CloseClientConnections()
		server.Close()
		if !errors.Is(err, station.ErrUnclear) || task != "" {
			t.Errorf("%s: Upload = %q, %v; want an unclear outcome", name, task, err)
			continue
		}
		// Far below the two minutes of the defaults, with room for a busy machine.
		if example.cause != nil && (!errors.Is(err, example.cause) || took > 10*time.Second) {
			t.Errorf("%s: %v after %v", name, err, took)
		}
	}
	_, err := Client{BaseURL: "http://127.0.0.1:1", Token: testToken}.Upload(context.Background(), "t", "scanbox-1.pdf", samplePDF(t), time.Time{})
	if !errors.Is(err, ErrUnreachable) || errors.Is(err, station.ErrUnclear) {
		t.Fatalf("no connection at all is not unclear: nothing was sent. Got %v", err)
	}
}

func TestASlowUploadThatKeepsMovingCompletes(t *testing.T) {
	const megabytes = 12
	received := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		buffer := make([]byte, 32<<10)
		for {
			count, err := request.Body.Read(buffer)
			received += count
			if err != nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		answering(http.StatusOK, jsonContent, `"`+testTask+`"`)(writer, request)
	}))
	defer server.Close()
	// Each pause in reading is a fiftieth of the stall time, while the whole upload lasts
	// several stall times.
	client := Client{BaseURL: server.URL, Token: testToken, stall: 500 * time.Millisecond, answerWait: 5 * time.Second}
	started := time.Now()
	task, err := client.Upload(context.Background(), "t", "scanbox-1.pdf", largePDF(t, megabytes), time.Time{})
	took := time.Since(started)
	if err != nil || task != testTask || received < megabytes<<20 {
		t.Fatalf("a slow upload: %q, %v, %d bytes received in %v", task, err, received, took)
	}
	if took < 3*client.stall {
		t.Fatalf("the upload took %v; it should outlast the stall time several times over to prove anything", took)
	}
}

func TestUploadSaysHowToGoOn(t *testing.T) {
	redirect := func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, loginPath, http.StatusFound)
	}
	cases := map[string]struct {
		handler http.HandlerFunc
		want    error
	}{
		"400, an invalid file":       {answering(http.StatusBadRequest, jsonContent, `{"document":["File type application/x-test not supported"]}`), station.ErrRejected},
		"413, too large for a proxy": {answering(http.StatusRequestEntityTooLarge, "text/html", "<title>413 Request Entity Too Large</title>"), station.ErrRejected},
		"415":                        {answering(http.StatusUnsupportedMediaType, jsonContent, `{"detail":"Unsupported media type"}`), station.ErrRejected},
		"422":                        {answering(http.StatusUnprocessableEntity, jsonContent, `{"detail":"no"}`), station.ErrRejected},
		"401, a refused token":       {answering(http.StatusUnauthorized, jsonContent, `{"detail":"Invalid token."}`), station.ErrDenied},
		"403, no permission":         {answering(http.StatusForbidden, "text/html", "Insufficient permissions"), station.ErrDenied},
		"a redirect":                 {redirect, station.ErrDenied},
		"a sign-in page":             {answering(http.StatusOK, "text/html", signInPage), station.ErrDenied},
		"404, no Paperless there":    {answering(http.StatusNotFound, "text/html", "<title>Not Found</title>"), station.ErrDenied},
	}
	for name, example := range cases {
		server := httptest.NewServer(example.handler)
		_, err := Client{BaseURL: server.URL, Token: testToken}.Upload(context.Background(), "t", "scanbox-1.pdf", samplePDF(t), time.Time{})
		server.Close()
		if !errors.Is(err, example.want) {
			t.Errorf("%s: %v, want %v", name, err, example.want)
		}
	}
	server := httptest.NewServer(answering(http.StatusTooManyRequests, "text/plain", "slow down"))
	defer server.Close()
	_, err := Client{BaseURL: server.URL, Token: testToken}.Upload(context.Background(), "t", "scanbox-1.pdf", samplePDF(t), time.Time{})
	if err == nil || errors.Is(err, station.ErrRejected) || errors.Is(err, station.ErrDenied) || errors.Is(err, station.ErrUnclear) {
		t.Fatalf("429 is only a reason to wait: %v", err)
	}
}

func TestAnUploadIsReadFromDiskAsItIsSent(t *testing.T) {
	const megabytes = 64
	received := int64(0)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received, _ = io.Copy(io.Discard, request.Body)
		answering(http.StatusOK, jsonContent, `"`+testTask+`"`)(writer, request)
	}))
	defer server.Close()
	path := largePDF(t, megabytes)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	task, err := Client{BaseURL: server.URL, Token: testToken}.Upload(context.Background(), "Gas bill", "scanbox-1.pdf", path, time.Time{})
	runtime.ReadMemStats(&after)
	if err != nil || task != testTask || received < megabytes<<20 {
		t.Fatalf("Upload = %q, %v; %d bytes received", task, err, received)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > megabytes<<20/4 {
		t.Fatalf("sending a %d MB document allocated %d MB; it should be read from disk as it goes", megabytes, allocated>>20)
	}
}

func TestCheckAddsNothing(t *testing.T) {
	var last received
	server := fakePaperless(t, &last)
	defer server.Close()
	if err := (Client{BaseURL: server.URL, Token: testToken}).Check(context.Background()); err != nil {
		t.Fatalf("Check with a good token: %v", err)
	}
	if last.content != "" {
		t.Fatal("Check uploaded a document")
	}
}

func TestProbeReadsTheVersionsPaperlessReports(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/" {
			http.Redirect(writer, request, "schema/view/", http.StatusFound)
			return
		}
		if request.URL.Path != probePath || request.Method != http.MethodGet {
			http.NotFound(writer, request)
			return
		}
		if request.Header.Get("Authorization") != "Token "+testToken {
			http.Error(writer, `{"detail":"Invalid token."}`, http.StatusUnauthorized)
			return
		}
		writer.Header().Set(versionHeader, "3.0.0")
		writer.Header().Set(apiVersionHeader, "10")
		writer.Write([]byte(`{"documents":"/api/documents/"}`))
	}))
	defer server.Close()
	info, err := Client{BaseURL: server.URL, Token: testToken}.Probe(context.Background())
	if err != nil || info.Version != "3.0.0" || info.APIVersion != "10" {
		t.Fatalf("Probe = %+v, %v", info, err)
	}
	if _, err := (Client{BaseURL: server.URL, Token: "wrong"}).Probe(context.Background()); !errors.Is(err, ErrTokenRejected) {
		t.Fatalf("a refused token gave %v", err)
	}
	if _, err := (Client{BaseURL: "http://127.0.0.1:1", Token: testToken}).Probe(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("no server gave %v", err)
	}
	followed := false
	redirecting := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/elsewhere/" {
			followed = true
			return
		}
		http.Redirect(writer, request, "/elsewhere/", http.StatusFound)
	}))
	defer redirecting.Close()
	_, err = Client{BaseURL: redirecting.URL, Token: testToken}.Probe(context.Background())
	if err == nil || !strings.Contains(err.Error(), "302") || followed {
		t.Fatalf("a redirect gave %v, followed: %v", err, followed)
	}
}

func TestErrorsArePlain(t *testing.T) {
	var last received
	server := fakePaperless(t, &last)
	defer server.Close()
	cases := []struct {
		client Client
		want   error
	}{
		{Client{BaseURL: server.URL, Token: "wrong"}, ErrTokenRejected},
		{Client{BaseURL: "", Token: testToken}, ErrNoAddress},
		{Client{BaseURL: server.URL, Token: ""}, ErrNoToken},
		{Client{BaseURL: "http://127.0.0.1:1", Token: testToken}, ErrUnreachable},
	}
	for _, example := range cases {
		if err := example.client.Check(context.Background()); !errors.Is(err, example.want) {
			t.Errorf("Check with %+v = %v, want %v", example.client, err, example.want)
		}
	}
	forbidden := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		http.Error(writer, "nope", http.StatusForbidden)
	}))
	defer forbidden.Close()
	_, err := Client{BaseURL: forbidden.URL, Token: testToken}.Upload(context.Background(), "t", "scanbox-1.pdf", samplePDF(t), time.Time{})
	if !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("403 gave %v", err)
	}
	if strings.Contains(ErrTokenRejected.Error(), testToken) {
		t.Fatal("an error message contains the token")
	}
}
