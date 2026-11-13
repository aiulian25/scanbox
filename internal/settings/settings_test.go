package settings

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultsNeedNoChanges(t *testing.T) {
	defaults := Defaults()
	if defaults.ResolutionDPI != 300 || defaults.PaperSize != PaperA4 || !defaults.Colour || defaults.IdleFinishMinutes != 2 {
		t.Fatalf("defaults changed: %+v", defaults)
	}
	if defaults.Buttons["pdf"] != AddColourPage || defaults.Buttons["send"] != AddGreyPage {
		t.Fatalf("button defaults changed: %v", defaults.Buttons)
	}
}

func TestSetChecksEveryField(t *testing.T) {
	cases := []struct {
		field, value string
		valid        bool
	}{
		{"paperless_url", "https://paperless.example.com/", true},
		{"paperless_url", "paperless.example.com", false},
		{"paperless_url", "", true},
		{"resolution_dpi", "600", true},
		{"resolution_dpi", "1200", false},
		{"paper_size", "letter", true},
		{"paper_size", "a3", false},
		{"grey_storage", "lossless", true},
		{"button.pdf", "finish_document", true},
		{"button.pdf", "explode", false},
		{"name_template", "Post {date}", true},
		{"name_template", "a/b", false},
		{"name_template", "Post\n{date}", false},
		{"name_template", "Post\t{date}", false},
		{"name_template", "Post \x1b[31m{date}", false},
		{"name_template", "Poșta {date} – €", true},
		{"button.power", "finish_document", false},
		{"button.", "finish_document", false},
		{"idle_finish_minutes", "0", false},
		{"keep_sent_days", "0", true},
		{"keep_sent_days", "30", true},
		{"keep_sent_days", "12", false},
		{"keep_sent_days", "seven", false},
		{"review_before_sending", "false", true},
		{"send_scan_date", "true", true},
		{"turn_pages", "true", true},
		{"time_zone", "Europe/Paris", true},
		{"time_zone", "Mars/Olympus", false},
		{"date_style", "iso", true},
		{"date_style", "mdy", false},
		{"time_style", "12", true},
		{"time_style", "25", false},
		{"reserve_mb", "500", true},
		{"reserve_mb", "300", false},
		{"reserve_mb", "0", false},
		{"unknown", "1", false},
	}
	for _, example := range cases {
		settings := Defaults()
		err := settings.Set(example.field, example.value)
		if (err == nil) != example.valid {
			t.Errorf("Set(%q, %q) = %v, valid %v", example.field, example.value, err, example.valid)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("Set(%q) error is not ErrInvalid: %v", example.field, err)
		}
	}
}

// The token goes with every request to the address, so the address may not send it across
// the internet readable, nor hold anything Paperless's own address doesn't.
func TestPaperlessAddressKeepsTheTokenSafe(t *testing.T) {
	cases := []struct {
		address string
		valid   bool
	}{
		{"https://paperless.example.com", true},
		{"https://paperless.example.com:8443/paperless", true},
		{"http://paperless.example.com", false},
		{"http://203.0.113.7:8000", false},
		{"http://[2001:db8::7]:8000", false},
		{"http://192.168.10.10:8000", true},
		{"http://10.0.0.5", true},
		{"http://127.0.0.1:8000", true},
		{"http://localhost:8000", true},
		{"http://paperless:8000", true},
		{"http://nas.local:8000", true},
		{"http://paperless.home.arpa", true},
		{"http://100.101.102.103:8000", true},
		{"http://[fd00::1]:8000", true},
		{"http://[fe80::1%25eth0]:8000", true},
		{"http://[::ffff:192.168.10.10]:8000", true},
		{"https://scanbox:secret@paperless.example.com", false},
		{"https://admin@paperless.example.com", false},
		{"https://paperless.example.com/?next=/documents", false},
		{"https://paperless.example.com/?", false},
		{"https://paperless.example.com/#/documents", false},
		{"ftp://paperless.example.com", false},
	}
	for _, example := range cases {
		settings := Defaults()
		err := settings.Set("paperless_url", example.address)
		if (err == nil) != example.valid {
			t.Errorf("Set(paperless_url, %q) = %v, valid %v", example.address, err, example.valid)
		}
		if err != nil && !errors.Is(err, ErrInvalid) {
			t.Errorf("Set(paperless_url, %q) error is not ErrInvalid: %v", example.address, err)
		}
	}
}

func TestOnlyLocalNamesCountAsLocal(t *testing.T) {
	for name, local := range map[string]bool{
		"paperless": true, "NAS.LOCAL": true, "nas.local.": true, "box.lan": true, "box.internal": true,
		"paperless.example.com": false, "local.example.com": false, "example.localhost.com": false, "": false,
	} {
		if IsLocalName(name) != local {
			t.Errorf("IsLocalName(%q) = %v, want %v", name, !local, local)
		}
	}
}

// Settings that can't be read don't stop the start, which would only have Docker start it
// again and again: the defaults are used and the file is kept beside them.
func TestDamagedSettingsAreSetAside(t *testing.T) {
	directory := t.TempDir()
	damaged := []byte(`{"paperless_url": "https://paperless.example.com", "resolution_dpi": 6`)
	if err := os.WriteFile(filepath.Join(directory, fileName), damaged, filePermissions); err != nil {
		t.Fatal(err)
	}
	store, err := Open(directory)
	if err != nil {
		t.Fatalf("damaged settings stopped the start: %v", err)
	}
	if store.Get().ResolutionDPI != Defaults().ResolutionDPI || store.Get().PaperlessURL != "" {
		t.Fatalf("after damaged settings the defaults should be in use: %+v", store.Get())
	}
	if !strings.Contains(store.Damaged, "settings.json.damaged") {
		t.Fatalf("Damaged = %q", store.Damaged)
	}
	kept, err := os.ReadFile(filepath.Join(directory, fileName+damagedSuffix))
	if err != nil || string(kept) != string(damaged) {
		t.Fatalf("the damaged file wasn't kept as it was: %q, %v", kept, err)
	}
	if _, err := store.Set("resolution_dpi", "600"); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(directory); err != nil || reopened.Damaged != "" || reopened.Get().ResolutionDPI != 600 {
		t.Fatalf("a change after the reset should be saved anew: %v", err)
	}
}

func TestHalfWrittenSettingsFilesAreRemoved(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{fileName + ".123456789", fileName + damagedSuffix} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte("{"), filePermissions); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, fileName+".123456789")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a half-written settings file was left behind")
	}
	if _, err := os.Stat(filepath.Join(directory, fileName+damagedSuffix)); err != nil {
		t.Fatal("the kept damaged settings were removed")
	}
}

func TestTrailingSlashIsDropped(t *testing.T) {
	settings := Defaults()
	if err := settings.Set("paperless_url", "https://paperless.example.com/"); err != nil {
		t.Fatal(err)
	}
	if settings.PaperlessURL != "https://paperless.example.com" {
		t.Fatalf("got %q", settings.PaperlessURL)
	}
}

func TestDocumentName(t *testing.T) {
	at := time.Date(2026, 11, 11, 19, 13, 52, 0, time.UTC)
	if name := Defaults().DocumentName(at); name != "Scan 11-11-2026 19-13" {
		t.Fatalf("got %q", name)
	}
	other := Defaults()
	other.Set("date_style", "mon-d-y")
	other.Set("time_style", "24-seconds")
	other.Set("time_zone", "Europe/Bucharest")
	if name := other.DocumentName(at); name != "Scan Nov 11 2026 21-13-52" {
		t.Fatalf("with other styles and zone: %q", name)
	}
}

func TestStoreKeepsChangesAcrossRestarts(t *testing.T) {
	directory := t.TempDir()
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set("resolution_dpi", "600"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Set("resolution_dpi", "999"); err == nil {
		t.Fatal("an invalid value was accepted")
	}
	reopened, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Get().ResolutionDPI; got != 600 {
		t.Fatalf("resolution after restart = %d", got)
	}
	info, err := os.Stat(filepath.Join(directory, fileName))
	if err != nil || info.Mode().Perm() != filePermissions {
		t.Fatalf("settings file: %v %v", info, err)
	}
	leftovers, _ := filepath.Glob(filepath.Join(directory, fileName+".*"))
	if len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
}

// Settings saved before version 3 with the old default of 10 minutes finish a document 2
// minutes after its last page; a time someone chose stays, and so does 10 chosen since.
func TestTheOldTenMinuteFinishBecomesTwo(t *testing.T) {
	for saved, want := range map[string]int{
		`{"version": 2, "idle_finish_minutes": 10}`: 2,
		`{"version": 2, "idle_finish_minutes": 5}`:  5,
		`{"version": 3, "idle_finish_minutes": 10}`: 10,
	} {
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, fileName), []byte(saved), filePermissions); err != nil {
			t.Fatal(err)
		}
		store, err := Open(directory)
		if err != nil {
			t.Fatal(err)
		}
		if got := store.Get().IdleFinishMinutes; got != want {
			t.Errorf("%s gives %d minutes, want %d", saved, got, want)
		}
	}
}

func TestOlderFileGetsNewButtonDefaults(t *testing.T) {
	directory := t.TempDir()
	old := `{"version":1,"paperless_url":"https://p.example","keep_sent_days":7,"buttons":{"pdf":"nothing"}}`
	if err := os.WriteFile(filepath.Join(directory, fileName), []byte(old), filePermissions); err != nil {
		t.Fatal(err)
	}
	store, err := Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	got := store.Get()
	if got.Version != currentVersion || got.Buttons["pdf"] != DoNothing || got.ResolutionDPI != 300 || got.KeepSentDays != 0 {
		t.Fatalf("upgrade wrong: %+v", got)
	}
	if got.ReserveMB != defaultReserveMB || defaultReserveMB != 200 {
		t.Fatalf("a file from before the reserve should keep 200 MB free: %d", got.ReserveMB)
	}
}

func TestGetReturnsACopy(t *testing.T) {
	store, _ := Open(t.TempDir())
	copied := store.Get()
	copied.Buttons["pdf"] = DoNothing
	if store.Get().Buttons["pdf"] == DoNothing {
		t.Fatal("changing a copy changed the store")
	}
}
