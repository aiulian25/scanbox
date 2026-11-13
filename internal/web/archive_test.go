package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"scanbox/internal/setup"
	"scanbox/internal/station"
)

// plantScans writes count sent scans, three a day going back from newest, as Scan Box
// keeps them on disk.
func plantScans(t *testing.T, directory string, count int, newest time.Time) {
	t.Helper()
	pdfBytes, thumbnail, preview := make([]byte, 30000), make([]byte, 8000), make([]byte, 60000)
	for index := range count {
		created := newest.Add(-time.Duration(index) * 8 * time.Hour)
		id := created.Format("02-01-2006_15-04-05")
		folder := filepath.Join(directory, "scans", created.Format("2006"), created.Format("01-2006"), created.Format("02-01-2006"), id)
		if err := os.MkdirAll(folder, 0o700); err != nil {
			t.Fatal(err)
		}
		info, err := json.Marshal(station.Document{ID: id, Name: "Gas bill " + id, Pages: 1, Colour: "greyscale", Created: created,
			State: station.Sent, SentAt: created.Add(time.Minute), DocumentID: 100 + index, Size: int64(len(pdfBytes))})
		if err != nil {
			t.Fatal(err)
		}
		files := map[string][]byte{"info.json": info, "document.pdf": pdfBytes, "page-001.jpg": thumbnail, "page-001.preview.jpg": preview}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(folder, name), content, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// One search box, the archive's filter beside the page; the header has none.
func TestThePageHasOneSearchBox(t *testing.T) {
	s := newSite(t, "")
	_, page := s.get(t, "/")
	if strings.Count(page, `type="search"`) != 1 || !strings.Contains(page, "data-archive-filter") {
		t.Fatal("the page should have one search box: the archive's filter")
	}
}

func TestUnreadableScansAndSettingsAreNamed(t *testing.T) {
	directory := t.TempDir()
	folder := filepath.Join(directory, "scans", "2026", "11-2026", "12-11-2026", "12-11-2026_12-00-00")
	if err := os.MkdirAll(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{filepath.Join(folder, "info.json"): `{"id": `, filepath.Join(directory, "settings.json"): `{"colour": tru`} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s := newSiteFor(t, Options{Access: setup.Open, Checker: nobody{}, DataDir: directory})
	_, page := s.get(t, "/")
	if !strings.Contains(page, "1 scan couldn&#39;t be read and isn&#39;t listed. The files are kept in scans/2026/11-2026/12-11-2026/12-11-2026_12-00-00") {
		t.Fatal("the archive doesn't name the scan that couldn't be read")
	}
	_, page = s.get(t, "/settings")
	if !strings.Contains(page, "Scan Box couldn&#39;t read settings.json") || !strings.Contains(page, "kept as settings.json.damaged.") {
		t.Fatal("the Settings page doesn't say the settings were reset")
	}
}

// Only the month on show lists its days; every other month is one link, so the archive
// stays small however many scans are kept.
func TestTheArchiveListsDaysOfTheShownMonthOnly(t *testing.T) {
	directory := t.TempDir()
	plantScans(t, directory, 300, time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC))
	s := newSiteFor(t, Options{Access: setup.Open, Checker: nobody{}, DataDir: directory})
	_, page := s.get(t, "/")
	archive := between(t, page, `<div class="tree">`, `<p class="archive-foot">`)
	if !strings.Contains(archive, `data-key="day-12-11-2026"`) || !strings.Contains(archive, `href="/scans?month=10-2026"`) {
		t.Fatalf("November should list its days and October be a link:\n%s", archive)
	}
	if strings.Contains(archive, `data-key="day-31-10-2026"`) || strings.Contains(archive, `href="/scans?day=31-10-2026"`) {
		t.Fatal("the days of a month not on show were listed")
	}
	_, page = s.get(t, "/scans?month=10-2026")
	archive = between(t, page, `<div class="tree">`, `<p class="archive-foot">`)
	if !strings.Contains(archive, `data-key="day-31-10-2026"`) || !strings.Contains(archive, `href="/scans?month=11-2026"`) {
		t.Fatalf("asked for October, the archive should list its days and November be a link:\n%s", archive)
	}
}
