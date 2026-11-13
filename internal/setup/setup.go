// Package setup reads what only the container can provide (secrets, the data volume,
// the time zone, access to the scanner) and says, with lines to copy, what is missing.
package setup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// Source says where a secret came from.
type Source string

const (
	FromFile     Source = "file"
	FromVariable Source = "variable"
	Missing      Source = ""

	fileSuffix          = "_FILE"
	mountPointField     = 4
	TokenVariable       = "PAPERLESS_TOKEN"
	PasswordVariable    = "SCANBOX_PASSWORD"
	TimeZoneVariable    = "TZ"
	ContainerDataFolder = "/data"
	Port                = "28761"

	// AllowNoPasswordVariable, set to allowNoPassword, serves the page without a password.
	AllowNoPasswordVariable = "SCANBOX_ALLOW_NO_PASSWORD"
	allowNoPassword         = "yes"
	// HostsVariable lists the names with a dot in them that the page answers to.
	HostsVariable = "SCANBOX_HOSTS"
	hostSeparator = ','
	// TrustedProxyVariable lists the reverse proxies, as addresses or ranges, whose
	// X-Forwarded-For and X-Forwarded-Proto the page believes.
	TrustedProxyVariable = "SCANBOX_TRUSTED_PROXY"
	// AddressVariable is the one address of the box Compose publishes the page on.
	AddressVariable = "SCANBOX_ADDRESS"
	// ImageVariable names, in .env, an image to run instead of the published one, such as
	// a local build that deploy/to-box.sh sends.
	ImageVariable = "SCANBOX_IMAGE"
	// PublishedImage is the released image Compose pulls; :0 is the newest 0.x release.
	PublishedImage = "ghcr.io/aiulian25/scanbox:0"
	// DataVolume is the volume at /data, made once with "docker volume create".
	DataVolume = "scanbox_scanbox-data"

	// tokenUser is the Paperless user a token should belong to: allowed to add documents,
	// and to view the tasks that say whether they were stored.
	tokenUser          = "a Paperless user that may add documents and view tasks"
	passwordTitle      = "Page password"
	passwordSecretName = "scanbox_password"
	passwordFileName   = "scanbox-password.txt"
	// ownershipCommands give a secret file on the host to the user the program runs as.
	ownershipCommands = "sudo chown %d %s\nsudo chmod 600 %s\ndocker compose up -d"
	// usbLines give the container the host's live USB devices, read-only, and leave to open
	// them: a scanner plugged in or back in later is seen at once. USB devices are major 189.
	// Scan Box opens one through the group of its device file, which no other device shares.
	usbLines = `    volumes:
      - /dev/bus/usb:/dev/bus/usb:ro
    device_cgroup_rules:
      - "c 189:* rw"
    group_add:
      - "${SCANNER_GID:?put SCANNER_GID, the group of the scanner's device file, in .env next to compose.yaml}"`
	// ownershipFix hands a volume over to the user Scan Box runs as, once.
	ownershipFix = "Scan Box runs as user %d, which may not write %s (%w). Once, on the box: docker run --rm -v %s:%s debian:trixie-slim chown -R %d:%d %s"
	// someVolume stands for the volume's name when the mount table doesn't give it.
	someVolume = "<the volume>"
	// mountRootField is the folder of the mounted file system that appears at the mount point.
	mountRootField = 3
	// Docker keeps a named volume's files in <data root>/volumes/<name>/_data.
	volumesFolder    = "volumes"
	volumeDataFolder = "_data"
)

// ErrEmptySecret is a secret file with nothing in it.
var ErrEmptySecret = errors.New("the file is empty")

// ReadSecret reads NAME_FILE if it is set, otherwise NAME. A file is preferred: it
// keeps the secret out of `docker inspect`. A file that can't be read, or is empty, is an
// error and never falls back to NAME: the secret was meant to come from that file.
func ReadSecret(name string) (string, Source, error) {
	if path := os.Getenv(name + fileSuffix); path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			return "", Missing, fmt.Errorf("%s%s points to %s, which can't be read: %w", name, fileSuffix, path, err)
		}
		secret := strings.TrimSpace(string(content))
		if secret == "" {
			return "", Missing, fmt.Errorf("%s%s points to %s: %w", name, fileSuffix, path, ErrEmptySecret)
		}
		return secret, FromFile, nil
	}
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value, FromVariable, nil
	}
	return "", Missing, nil
}

// Access says who may use the page. The zero value is Locked, so a page nobody decided
// about stays shut.
type Access int

const (
	// Locked serves nothing but a notice saying why.
	Locked Access = iota
	// BySignIn serves the page to whoever signs in with the password.
	BySignIn
	// Open serves the page to everyone who can reach it.
	Open
)

// PageAccess decides who may use the page, from what ReadSecret found for its password.
// With no password the page is locked, unless SCANBOX_ALLOW_NO_PASSWORD asks to run without
// one. A password that is set but can't be used always locks it. For a locked page the
// Check says why, and what to do.
func PageAccess(source Source, problem error) (Access, Check) {
	if problem != nil {
		return Locked, unusablePassword(problem)
	}
	if source != Missing {
		return BySignIn, Check{}
	}
	if os.Getenv(AllowNoPasswordVariable) == allowNoPassword {
		return Open, Check{}
	}
	return Locked, noPassword()
}

func noPassword() Check {
	return Check{
		Title:  passwordTitle,
		Detail: "It has no password yet.",
		Help: fmt.Sprintf("Put a password in a file next to your compose file, for example %s, add these lines, then run docker compose up -d. "+
			"To use the page without a password, on a network where you trust everyone, set %s: \"%s\" under environment instead.",
			passwordFileName, AllowNoPasswordVariable, allowNoPassword),
		Snippet: passwordSnippet(),
	}
}

// unusablePassword says what is wrong with a password that is set but can't be used.
func unusablePassword(problem error) Check {
	check := Check{Title: passwordTitle, Detail: "Its password file can't be read.", Help: problem.Error() + "."}
	switch {
	case errors.Is(problem, ErrEmptySecret):
		check.Detail = "Its password file is empty."
		check.Help = "Write a password into the file, then run docker compose up -d."
	case errors.Is(problem, fs.ErrNotExist):
		check.Detail = "Its password file isn't there."
		check.Help = problem.Error() + ". Check these lines in your compose file, then run docker compose up -d."
		check.Snippet = passwordSnippet()
	case errors.Is(problem, fs.ErrPermission):
		user := os.Getuid()
		check.Detail = "Scan Box isn't allowed to read its password file."
		check.Help = fmt.Sprintf("The file must be readable by user number %d, which Scan Box runs as. On the box, next to your compose file:", user)
		check.Snippet = fmt.Sprintf(ownershipCommands, user, passwordFileName, passwordFileName)
	}
	return check
}

func passwordSnippet() string {
	return secretSnippet(PasswordVariable, passwordSecretName, "./"+passwordFileName)
}

// Hosts gives the names in SCANBOX_HOSTS, separated by commas or spaces: the names with a
// dot in them that the page should answer to, such as a reverse proxy's.
func Hosts() []string {
	return listed(strings.ToLower(os.Getenv(HostsVariable)))
}

// TrustedProxies gives the addresses and ranges in SCANBOX_TRUSTED_PROXY, and the entries
// that are neither, which are left out.
func TrustedProxies() ([]netip.Prefix, []string) {
	var proxies []netip.Prefix
	var unreadable []string
	for _, entry := range listed(os.Getenv(TrustedProxyVariable)) {
		if address, err := netip.ParseAddr(entry); err == nil {
			address = address.Unmap()
			proxies = append(proxies, netip.PrefixFrom(address, address.BitLen()))
			continue
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			proxies = append(proxies, prefix.Masked())
			continue
		}
		unreadable = append(unreadable, entry)
	}
	return proxies, unreadable
}

// listed splits a variable's value at commas and spaces.
func listed(value string) []string {
	return strings.FieldsFunc(value, func(character rune) bool {
		return character == hostSeparator || unicode.IsSpace(character)
	})
}

// IsMountPoint reads /proc/self/mountinfo to tell a mounted volume from the container's own layer.
func IsMountPoint(mountInfo io.Reader, path string) bool {
	lines := bufio.NewScanner(mountInfo)
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) > mountPointField && fields[mountPointField] == path {
			return true
		}
	}
	return false
}

// VolumeName gives the name of the Docker volume mounted at path, or "" if what is there
// isn't one. Docker keeps a named volume's files in <data root>/volumes/<name>/_data, which
// the mount table shows as the mount's root.
func VolumeName(mountInfo io.Reader, path string) string {
	lines := bufio.NewScanner(mountInfo)
	for lines.Scan() {
		fields := strings.Fields(lines.Text())
		if len(fields) <= mountPointField || fields[mountPointField] != path {
			continue
		}
		root := fields[mountRootField]
		folder := filepath.Dir(root)
		if filepath.Base(root) != volumeDataFolder || filepath.Base(filepath.Dir(folder)) != volumesFolder {
			return ""
		}
		return filepath.Base(folder)
	}
	return ""
}

// DataAdvice explains a data folder Scan Box may not write, such as a volume made while it
// still ran as root, with the one command that hands it over. Other problems pass as they are.
func DataAdvice(path string, problem error) error {
	if !errors.Is(problem, fs.ErrPermission) {
		return problem
	}
	volume := someVolume
	if mountInfo, err := os.Open("/proc/self/mountinfo"); err == nil {
		if name := VolumeName(mountInfo, path); name != "" {
			volume = name
		}
		mountInfo.Close()
	}
	user := os.Getuid()
	return fmt.Errorf(ownershipFix, user, path, problem, volume, path, user, user, path)
}

// DataIsVolume says whether /data survives the container being replaced.
func DataIsVolume(path string) bool {
	mountInfo, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	defer mountInfo.Close()
	return IsMountPoint(mountInfo, path)
}

type Facts struct {
	ScannerName  string
	TokenSource  Source
	TokenProblem string
	// TasksDenied says Paperless takes uploads from the token, but its user may not view
	// the tasks that say whether they were stored.
	TasksDenied    bool
	PasswordSource Source
	DataIsVolume   bool
}

type Check struct {
	Title   string
	OK      bool
	Detail  string
	Help    string
	Snippet string
}

func Checks(facts Facts) []Check {
	return []Check{
		scannerCheck(facts),
		tokenCheck(facts),
		passwordCheck(facts),
		dataCheck(facts),
	}
}

func MissingCount(checks []Check) int {
	count := 0
	for _, check := range checks {
		if !check.OK {
			count++
		}
	}
	return count
}

func scannerCheck(facts Facts) Check {
	check := Check{Title: "Scanner access", OK: facts.ScannerName != "", Detail: facts.ScannerName + " found"}
	if check.OK {
		return check
	}
	check.Detail = "No scanner found"
	check.Help = "Check that the scanner is plugged in and switched on. If it is, the container can't see USB devices yet; add these lines. " +
		"A devices: entry for /dev/bus/usb copies only the devices there at the start, so replace one if you have it:"
	check.Snippet = usbLines
	return check
}

func tokenCheck(facts Facts) Check {
	check := Check{Title: "Paperless token", OK: facts.TokenSource != Missing && facts.TokenProblem == "" && !facts.TasksDenied}
	if check.OK {
		check.Detail = "Set, " + sourceWords(facts.TokenSource)
		return check
	}
	if facts.TokenSource != Missing && facts.TokenProblem != "" {
		check.Detail = "Not accepted"
		check.Help = facts.TokenProblem + ". Make a new token in Paperless for " + tokenUser + ", and put it in the token file."
		return check
	}
	if facts.TokenSource != Missing {
		check.Detail = "Can't view tasks"
		check.Help = "Paperless takes uploads from this token, but its user may not view tasks, so Scan Box can't confirm that a scan was stored. " +
			"In Paperless, open Users & Groups, edit that user and tick View for PaperlessTask."
		return check
	}
	check.Detail = "Missing"
	check.Help = "Put the token of " + tokenUser + " in a file next to your compose file, for example paperless-token.txt, then add:"
	check.Snippet = secretSnippet(TokenVariable, "paperless_token", "./paperless-token.txt")
	return check
}

func passwordCheck(facts Facts) Check {
	check := Check{Title: passwordTitle, OK: facts.PasswordSource != Missing}
	if check.OK {
		check.Detail = "Set, " + sourceWords(facts.PasswordSource)
		return check
	}
	check.Detail = "Missing"
	check.Help = "Without it, anyone on your network can open this page. Put a password in a file next to your compose file, for example scanbox-password.txt, then add:"
	check.Snippet = passwordSnippet()
	return check
}

// dataVolumeSection declares the volume at /data external: Compose never deletes it, not
// even with "docker compose down -v", so the scans go only when someone removes the volume.
const dataVolumeSection = `volumes:
  scanbox-data:
    external: true
    name: ` + DataVolume

func dataCheck(facts Facts) Check {
	check := Check{Title: "Settings and scans kept", OK: facts.DataIsVolume, Detail: "Stored in a volume"}
	if check.OK {
		return check
	}
	check.Detail = "Lost on update"
	check.Help = "Settings and waiting scans are inside the container and go when it is replaced. Make the volume once with docker volume create " +
		DataVolume + ", then add:"
	check.Snippet = "    volumes:\n      - scanbox-data:/data\n\n" + dataVolumeSection
	return check
}

func sourceWords(source Source) string {
	if source == FromFile {
		return "from a file"
	}
	return "from a variable"
}

func secretSnippet(variable, secret, file string) string {
	return fmt.Sprintf(`    environment:
      %s%s: /run/secrets/%s
    secrets:
      - %s

secrets:
  %s:
    file: %s`, variable, fileSuffix, secret, secret, secret, file)
}

// FullCompose is a complete compose file for a new box. Like compose.yaml, it runs the
// published image, or the one SCANBOX_IMAGE in .env names.
func FullCompose() string {
	image := "${" + ImageVariable + ":-" + PublishedImage + "}"
	return fmt.Sprintf(`services:
  scanbox:
    image: %s
    container_name: scanbox
    restart: unless-stopped
    env_file: .env
    ports:
      - "${%s:?put %s, the address to publish the page on, in .env next to compose.yaml}:%s:%s"
    volumes:
      - scanbox-data:/data
      - /dev/bus/usb:/dev/bus/usb:ro
    device_cgroup_rules:
      - "c 189:* rw"
    group_add:
      - "${SCANNER_GID:?put SCANNER_GID, the group of the scanner's device file, in .env next to compose.yaml}"
    environment:
      %s%s: /run/secrets/paperless_token
      %s%s: /run/secrets/scanbox_password
      GOMEMLIMIT: 640MiB
    secrets:
      - paperless_token
      - scanbox_password
    read_only: true
    tmpfs:
      - /tmp
    cap_drop: [ALL]
    security_opt: [no-new-privileges:true]
    mem_limit: 768m
    pids_limit: 128
    logging:
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"

secrets:
  paperless_token:
    file: ./paperless-token.txt
  scanbox_password:
    file: ./scanbox-password.txt

%s`, image, AddressVariable, AddressVariable, Port, Port, TokenVariable, fileSuffix, PasswordVariable, fileSuffix, dataVolumeSection)
}
