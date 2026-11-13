// Package settings holds everything people change on the Settings page, with
// defaults that suit documents, and keeps it in one JSON file in the data folder.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"scanbox/internal/clock"
	"scanbox/internal/scanner"
)

const (
	currentVersion     = 3
	oldDefaultKeepDays = 7
	// A document finishes by itself this many minutes after its last page; 10 until version 3.
	defaultIdleMinutes    = 2
	oldDefaultIdleMinutes = 10
	fileName              = "settings.json"
	damagedSuffix         = ".damaged"
	filePermissions       = 0o600
	maximumNameLength     = 120
	minimumIdleMinutes    = 1
	maximumIdleMinutes    = 240
	datePlaceholder       = "{date}"
	timePlaceholder       = "{time}"
	// defaultReserveMB leaves room for the system and a few scans' worth beyond it.
	defaultReserveMB = 200
)

type PaperSize string

const (
	PaperA4     PaperSize = "a4"
	PaperLetter PaperSize = "letter"
	PaperGlass  PaperSize = "glass"
)

// GreyStorage says how greyscale pages are kept: JPEG, or lossless and about 3 times larger.
type GreyStorage string

const (
	GreyCompact  GreyStorage = "compact"
	GreyLossless GreyStorage = "lossless"
)

// Action is what a button on the scanner, or on the page, does.
type Action string

const (
	AddColourPage  Action = "add_colour_page"
	AddGreyPage    Action = "add_grey_page"
	FinishDocument Action = "finish_document"
	OneColourPage  Action = "one_colour_page"
	OneGreyPage    Action = "one_grey_page"
	DoNothing      Action = "nothing"
)

// Button names a front button of the scanner, as the scanner package reports it.
type Button string

type Settings struct {
	Version           int               `json:"version"`
	PaperlessURL      string            `json:"paperless_url"`
	ResolutionDPI     int               `json:"resolution_dpi"`
	PaperSize         PaperSize         `json:"paper_size"`
	GreyStorage       GreyStorage       `json:"grey_storage"`
	Colour            bool              `json:"colour"`
	Buttons           map[Button]Action `json:"buttons"`
	NameTemplate      string            `json:"name_template"`
	IdleFinishMinutes int               `json:"idle_finish_minutes"`
	KeepSentDays      int               `json:"keep_sent_days"`
	// ReviewBeforeSending keeps each finished scan until someone names it and sends it.
	ReviewBeforeSending bool `json:"review_before_sending"`
	// TurnPages turns every scan upside down, for pages put on the glass the other way round.
	TurnPages bool   `json:"turn_pages"`
	TimeZone  string `json:"time_zone"`
	DateStyle string `json:"date_style"`
	TimeStyle string `json:"time_style"`
	// ReserveMB is the space, in megabytes, scanning leaves free on the box.
	ReserveMB int `json:"reserve_mb"`
	// SendScanDate dates documents in Paperless by the day they were scanned. Left off,
	// Paperless dates them itself: by the date printed in them, or the day they arrive.
	SendScanDate bool `json:"send_scan_date"`
}

var (
	Resolutions = []int{150, 300, 600}
	// KeepChoices are the offered ways to keep sent scans, in days; 0 keeps them always.
	KeepChoices = []int{0, 7, 30, 90, 365}
	// ReserveChoices are the offered amounts of space to keep free, in megabytes.
	ReserveChoices = []int{100, 200, 500, 1000, 2000}
	PaperSizes     = []PaperSize{PaperA4, PaperLetter, PaperGlass}
	Actions        = []Action{AddColourPage, AddGreyPage, FinishDocument, OneColourPage, OneGreyPage, DoNothing}
)

// Defaults are chosen so that most people never open the Settings page.
func Defaults() Settings {
	return Settings{
		Version:       currentVersion,
		ResolutionDPI: 300,
		PaperSize:     PaperA4,
		GreyStorage:   GreyCompact,
		Colour:        true,
		Buttons: map[Button]Action{
			"pdf":        AddColourPage,
			"pdf_finish": FinishDocument,
			"auto_scan":  OneGreyPage,
			"copy":       OneColourPage,
			"send":       AddGreyPage,
		},
		NameTemplate:        "Scan " + datePlaceholder + " " + timePlaceholder,
		IdleFinishMinutes:   defaultIdleMinutes,
		KeepSentDays:        0,
		ReviewBeforeSending: true,
		TimeZone:            clock.DefaultTimeZone,
		DateStyle:           clock.DefaultDateStyle,
		TimeStyle:           clock.DefaultTimeStyle,
		ReserveMB:           defaultReserveMB,
	}
}

// ErrInvalid wraps every refusal of a value, so the page can show the reason next to the field.
var ErrInvalid = errors.New("invalid setting")

func invalid(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, arguments...))
}

// Set changes one field by the name the Settings page uses for it.
func (s *Settings) Set(field, value string) error {
	value = strings.TrimSpace(value)
	if button, isButton := strings.CutPrefix(field, "button."); isButton {
		return s.setButton(Button(button), Action(value))
	}
	switch field {
	case "paperless_url":
		return s.setPaperlessURL(value)
	case "resolution_dpi":
		return s.setResolution(value)
	case "paper_size":
		return s.setPaperSize(PaperSize(value))
	case "grey_storage":
		return s.setGreyStorage(GreyStorage(value))
	case "colour":
		s.Colour = value == "true"
		return nil
	case "review_before_sending":
		s.ReviewBeforeSending = value == "true"
		return nil
	case "send_scan_date":
		s.SendScanDate = value == "true"
		return nil
	case "turn_pages":
		s.TurnPages = value == "true"
		return nil
	case "time_zone":
		if _, err := clock.LoadZone(value); err != nil {
			return invalid("choose a time zone from the list")
		}
		s.TimeZone = value
		return nil
	case "date_style":
		if !clock.ValidDateStyle(value) {
			return invalid("choose a date style from the list")
		}
		s.DateStyle = value
		return nil
	case "time_style":
		if !clock.ValidTimeStyle(value) {
			return invalid("choose a time style from the list")
		}
		s.TimeStyle = value
		return nil
	case "name_template":
		return s.setNameTemplate(value)
	case "idle_finish_minutes":
		return s.setWholeNumber(&s.IdleFinishMinutes, value, minimumIdleMinutes, maximumIdleMinutes)
	case "keep_sent_days":
		return s.setKeepDays(value)
	case "reserve_mb":
		return s.setReserve(value)
	}
	return invalid("unknown setting %q", field)
}

func (s *Settings) setReserve(value string) error {
	megabytes, err := strconv.Atoi(value)
	if err != nil || !contains(ReserveChoices, megabytes) {
		return invalid("choose one of the offered amounts")
	}
	s.ReserveMB = megabytes
	return nil
}

func (s *Settings) setPaperlessURL(value string) error {
	value = strings.TrimRight(value, "/")
	if value == "" {
		s.PaperlessURL = ""
		return nil
	}
	address, err := url.Parse(value)
	if err != nil || address.Host == "" || (address.Scheme != "http" && address.Scheme != "https") {
		return invalid("enter the address as it appears in your browser, for example https://paperless.example.com")
	}
	if address.User != nil {
		return invalid("leave the user name and password out of the address: Scan Box signs in with the token")
	}
	if address.RawQuery != "" || address.ForceQuery || address.Fragment != "" {
		return invalid("leave out anything from ? or # on: the address ends with the host, or with the folder Paperless is in")
	}
	if address.Scheme == "http" && !isLocalHost(address.Hostname()) {
		return invalid("use https for an address beyond your own network, or the token crosses the internet readable on the way. " +
			"http is fine on your own network, for example http://192.168.10.10:8000")
	}
	s.PaperlessURL = value
	return nil
}

// localNameSuffixes end names that only a home or office network gives out, never the
// public internet.
var localNameSuffixes = []string{".localhost", ".local", ".lan", ".home.arpa", ".internal"}

// temporaryName is a settings file not yet renamed into place: its name, a dot and the
// digits os.CreateTemp added.
var temporaryName = regexp.MustCompile(`^` + regexp.QuoteMeta(fileName) + `\.[0-9]+$`)

// sharedAddressSpace is the range carriers and VPNs such as Tailscale use (RFC 6598);
// like private ranges, it isn't reached across the internet.
var sharedAddressSpace = netip.MustParsePrefix("100.64.0.0/10")

// IsLocalName says whether only the box's own network can give out a name: one without a
// dot, or one with a local ending.
func IsLocalName(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if name != "" && !strings.Contains(name, ".") {
		return true
	}
	return slices.ContainsFunc(localNameSuffixes, func(suffix string) bool { return strings.HasSuffix(name, suffix) })
}

// isLocalHost says whether a host can only be on the box's own network: a loopback,
// private, link-local or shared address, or a local name.
func isLocalHost(host string) bool {
	address, err := netip.ParseAddr(host)
	if err != nil {
		return IsLocalName(host)
	}
	address = address.Unmap()
	return address.IsLoopback() || address.IsPrivate() || address.IsLinkLocalUnicast() || sharedAddressSpace.Contains(address)
}

func (s *Settings) setResolution(value string) error {
	resolution, err := strconv.Atoi(value)
	if err != nil || !contains(Resolutions, resolution) {
		return invalid("choose 150, 300 or 600 dpi")
	}
	s.ResolutionDPI = resolution
	return nil
}

func (s *Settings) setPaperSize(size PaperSize) error {
	if !contains(PaperSizes, size) {
		return invalid("unknown paper size")
	}
	s.PaperSize = size
	return nil
}

func (s *Settings) setGreyStorage(storage GreyStorage) error {
	if storage != GreyCompact && storage != GreyLossless {
		return invalid("unknown greyscale storage")
	}
	s.GreyStorage = storage
	return nil
}

func (s *Settings) setButton(button Button, action Action) error {
	isFrontButton := slices.ContainsFunc(scanner.FrontButtons, func(front scanner.ButtonLabel) bool { return Button(front.Name) == button })
	if !isFrontButton {
		return invalid("the scanner has no button %q", button)
	}
	if !contains(Actions, action) {
		return invalid("unknown action")
	}
	s.Buttons[button] = action
	return nil
}

func (s *Settings) setNameTemplate(value string) error {
	if err := CheckName(value); err != nil {
		return err
	}
	s.NameTemplate = value
	return nil
}

// CheckName says whether a document name, or a name template, can be used.
func CheckName(name string) error {
	if strings.TrimSpace(name) == "" || len(name) > maximumNameLength {
		return invalid("the name needs between 1 and %d characters", maximumNameLength)
	}
	if strings.ContainsFunc(name, unicode.IsControl) {
		return invalid("the name can't contain line breaks, tabs or other control characters")
	}
	if strings.ContainsAny(name, `/\:*?"<>|`) {
		return invalid(`the name can't contain / \ : * ? " < > |`)
	}
	return nil
}

func (s *Settings) setKeepDays(value string) error {
	days, err := strconv.Atoi(value)
	if err != nil || !contains(KeepChoices, days) {
		return invalid("choose one of the offered periods")
	}
	s.KeepSentDays = days
	return nil
}

func (s *Settings) setWholeNumber(target *int, value string, minimum, maximum int) error {
	number, err := strconv.Atoi(value)
	if err != nil || number < minimum || number > maximum {
		return invalid("enter a whole number from %d to %d", minimum, maximum)
	}
	*target = number
	return nil
}

// Clock writes dates and times the way these settings say.
func (s Settings) Clock() clock.Clock {
	return clock.New(s.TimeZone, s.DateStyle, s.TimeStyle)
}

// DocumentName fills the name template in for a document finished at the given time.
func (s Settings) DocumentName(at time.Time) string {
	date, clockTime := s.Clock().ForName(at)
	name := strings.ReplaceAll(s.NameTemplate, datePlaceholder, date)
	return strings.ReplaceAll(name, timePlaceholder, clockTime)
}

func (s Settings) clone() Settings {
	copied := s
	copied.Buttons = make(map[Button]Action, len(s.Buttons))
	for button, action := range s.Buttons {
		copied.Buttons[button] = action
	}
	return copied
}

func contains[T comparable](list []T, wanted T) bool {
	for _, item := range list {
		if item == wanted {
			return true
		}
	}
	return false
}

// Store keeps the settings in memory and on disk. It is safe for use from several goroutines.
type Store struct {
	path    string
	mutex   sync.Mutex
	current Settings
	// Fresh is true when no settings were saved before: a new install.
	Fresh bool
	// Damaged says why the saved settings couldn't be read, when they couldn't: the store
	// then started from the defaults, and the file was kept beside them.
	Damaged string
}

// Open reads the settings from the data folder, or starts from the defaults. Settings
// that can't be read are set aside as settings.json.damaged rather than stopping the start,
// which would only have Docker start it again and again.
func Open(dataDirectory string) (*Store, error) {
	store := &Store{path: filepath.Join(dataDirectory, fileName), current: Defaults()}
	removeLeftovers(dataDirectory)
	content, err := os.ReadFile(store.path)
	if errors.Is(err, os.ErrNotExist) {
		store.Fresh = true
		return store, nil
	}
	if err != nil {
		return nil, err
	}
	loaded := Defaults()
	if err := json.Unmarshal(content, &loaded); err != nil {
		if err := store.setAside(err); err != nil {
			return nil, err
		}
		return store, nil
	}
	store.current = upgrade(loaded)
	return store, nil
}

// setAside keeps unreadable settings as settings.json.damaged and notes why.
func (s *Store) setAside(problem error) error {
	if err := os.Rename(s.path, s.path+damagedSuffix); err != nil {
		return fmt.Errorf("%s: %w", s.path, problem)
	}
	s.Damaged = fmt.Sprintf("Scan Box couldn't read %s (%v), so it started from the defaults; the old file is kept as %s%s",
		fileName, problem, fileName, damagedSuffix)
	return nil
}

// removeLeftovers deletes settings files a stop left half-written: settings.json, a dot
// and the digits os.CreateTemp added.
func removeLeftovers(dataDirectory string) {
	matches, _ := filepath.Glob(filepath.Join(dataDirectory, fileName+".*"))
	for _, match := range matches {
		if temporaryName.MatchString(filepath.Base(match)) {
			os.Remove(match)
		}
	}
}

// upgrade brings settings saved by an older version up to date; buttons new to this
// version keep their defaults because loading starts from Defaults(). Version 2 keeps
// sent scans always, so the old default of 7 days becomes "always". Version 3 finishes a
// document 2 minutes after its last page, so the old default of 10 minutes becomes 2.
func upgrade(loaded Settings) Settings {
	if loaded.Version < 2 && loaded.KeepSentDays == oldDefaultKeepDays {
		loaded.KeepSentDays = 0
	}
	if loaded.Version < 3 && loaded.IdleFinishMinutes == oldDefaultIdleMinutes {
		loaded.IdleFinishMinutes = defaultIdleMinutes
	}
	loaded.Version = currentVersion
	return loaded
}

func (s *Store) Get() Settings {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.current.clone()
}

// Set changes one field and saves the file before returning.
func (s *Store) Set(field, value string) (Settings, error) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	changed := s.current.clone()
	if err := changed.Set(field, value); err != nil {
		return s.current.clone(), err
	}
	if err := writeAtomically(s.path, changed); err != nil {
		return s.current.clone(), err
	}
	s.current = changed
	return changed.clone(), nil
}

// writeAtomically writes a temporary file and renames it, so a power cut never leaves half a file.
func writeAtomically(path string, settings Settings) error {
	content, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), fileName+".*")
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
	// The folder is synced too, so a power cut can't undo the rename.
	if folder, err := os.Open(filepath.Dir(path)); err == nil {
		folder.Sync()
		folder.Close()
	}
	return nil
}
