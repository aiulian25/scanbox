package setup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestReadSecretPrefersTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(TokenVariable, "from-variable")
	t.Setenv(TokenVariable+fileSuffix, path)
	value, source, err := ReadSecret(TokenVariable)
	if err != nil || value != "from-file" || source != FromFile {
		t.Fatalf("got %q %q %v", value, source, err)
	}
	t.Setenv(TokenVariable+fileSuffix, "")
	if value, source, _ = ReadSecret(TokenVariable); value != "from-variable" || source != FromVariable {
		t.Fatalf("got %q %q", value, source)
	}
	t.Setenv(TokenVariable+fileSuffix, filepath.Join(t.TempDir(), "absent"))
	if _, _, err = ReadSecret(TokenVariable); err == nil {
		t.Fatal("a missing secret file gave no error")
	}
}

func TestAnEmptySecretFileIsNotASecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "password.txt")
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(PasswordVariable+fileSuffix, path)
	value, source, err := ReadSecret(PasswordVariable)
	if !errors.Is(err, ErrEmptySecret) || value != "" || source != Missing {
		t.Fatalf("an empty secret file gave %q %q %v", value, source, err)
	}
	if !strings.Contains(err.Error(), PasswordVariable+fileSuffix) || !strings.Contains(err.Error(), path) {
		t.Fatalf("the error doesn't name the variable and the file: %v", err)
	}
}

// secretFile sets NAME_FILE to a file holding the given text, and returns its path.
func secretFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(name+fileSuffix, path)
	return path
}

func TestThePageIsLockedWithoutAPasswordItCanUse(t *testing.T) {
	t.Setenv(PasswordVariable, "")
	t.Setenv(PasswordVariable+fileSuffix, "")
	t.Setenv(AllowNoPasswordVariable, "")

	_, source, problem := ReadSecret(PasswordVariable)
	access, notice := PageAccess(source, problem)
	if access != Locked || !strings.Contains(notice.Snippet, "SCANBOX_PASSWORD_FILE: /run/secrets/scanbox_password") || !strings.Contains(notice.Help, AllowNoPasswordVariable) {
		t.Fatalf("no password: %v %+v", access, notice)
	}

	t.Setenv(AllowNoPasswordVariable, "true")
	if access, _ := PageAccess(source, problem); access != Locked {
		t.Fatalf("only %q opens the page, but %q gave %v", allowNoPassword, "true", access)
	}
	t.Setenv(AllowNoPasswordVariable, allowNoPassword)
	if access, notice := PageAccess(source, problem); access != Open || notice != (Check{}) {
		t.Fatalf("no password, and running without one asked for: %v %+v", access, notice)
	}

	secretFile(t, PasswordVariable, "")
	_, source, problem = ReadSecret(PasswordVariable)
	if access, notice := PageAccess(source, problem); access != Locked || !strings.Contains(notice.Detail, "empty") {
		t.Fatalf("an empty password file, though running without one is asked for: %v %+v", access, notice)
	}

	absent := filepath.Join(t.TempDir(), "absent")
	t.Setenv(PasswordVariable+fileSuffix, absent)
	_, source, problem = ReadSecret(PasswordVariable)
	if access, notice := PageAccess(source, problem); access != Locked || !strings.Contains(notice.Help, absent) || notice.Snippet == "" {
		t.Fatalf("a password file that isn't there: %v %+v", access, notice)
	}

	secretFile(t, PasswordVariable, "secret\n")
	t.Setenv(AllowNoPasswordVariable, "")
	_, source, problem = ReadSecret(PasswordVariable)
	if access, notice := PageAccess(source, problem); access != BySignIn || notice != (Check{}) {
		t.Fatalf("a password that can be read: %v %+v", access, notice)
	}
}

func TestAPasswordFileScanBoxMayNotReadLocksThePage(t *testing.T) {
	t.Setenv(PasswordVariable, "")
	t.Setenv(AllowNoPasswordVariable, allowNoPassword)
	unreadable := secretFile(t, PasswordVariable, "secret")
	if err := os.Chmod(unreadable, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.ReadFile(unreadable); err == nil {
		t.Skip("this user reads files whatever their mode, as root does while the image is built")
	}
	_, source, problem := ReadSecret(PasswordVariable)
	access, notice := PageAccess(source, problem)
	if access != Locked || !strings.Contains(notice.Snippet, "chown "+strconv.Itoa(os.Getuid())) {
		t.Fatalf("a password file that can't be read: %v %+v", access, notice)
	}
}

func TestTheZeroAccessIsLocked(t *testing.T) {
	var access Access
	if access != Locked {
		t.Fatal("a page nobody decided about must be locked")
	}
}

func TestHostsAreReadFromTheContainer(t *testing.T) {
	t.Setenv(HostsVariable, " Scan.Example.org, box.example.net\tthird.example ")
	hosts := Hosts()
	if strings.Join(hosts, "|") != "scan.example.org|box.example.net|third.example" {
		t.Fatalf("hosts: %q", hosts)
	}
	t.Setenv(HostsVariable, "")
	if len(Hosts()) != 0 {
		t.Fatal("no hosts were listed, but some came out")
	}
}

func TestTrustedProxiesAreAddressesOrRanges(t *testing.T) {
	t.Setenv(TrustedProxyVariable, "172.18.0.2, 10.0.0.0/8 nginx\t::1 ::ffff:192.168.10.5")
	proxies, unreadable := TrustedProxies()
	got := make([]string, len(proxies))
	for index, proxy := range proxies {
		got[index] = proxy.String()
	}
	if strings.Join(got, " ") != "172.18.0.2/32 10.0.0.0/8 ::1/128 192.168.10.5/32" || strings.Join(unreadable, " ") != "nginx" {
		t.Fatalf("proxies %v, left out %v", got, unreadable)
	}
	t.Setenv(TrustedProxyVariable, "")
	if proxies, unreadable := TrustedProxies(); len(proxies) != 0 || len(unreadable) != 0 {
		t.Fatal("none listed, but some came out")
	}
}

func TestIsMountPoint(t *testing.T) {
	mountInfo := `612 589 0:52 / / rw,relatime - overlay overlay rw
640 612 259:2 /var/lib/docker/volumes/scanbox-data/_data /data rw,relatime - ext4 /dev/nvme0n1p2 rw
`
	if !IsMountPoint(strings.NewReader(mountInfo), "/data") || IsMountPoint(strings.NewReader(mountInfo), "/tmp") {
		t.Fatal("mount points read wrongly")
	}
}

func TestTheDataVolumeIsNamedFromTheMountTable(t *testing.T) {
	mountInfo := `612 589 0:52 / / rw,relatime - overlay overlay rw
640 612 259:2 /var/lib/docker/volumes/scanbox_scanbox-data/_data /data rw,relatime - ext4 /dev/nvme0n1p2 rw
641 612 259:2 /home/someone/scans /bound rw,relatime - ext4 /dev/nvme0n1p2 rw
`
	if name := VolumeName(strings.NewReader(mountInfo), "/data"); name != "scanbox_scanbox-data" {
		t.Fatalf("volume %q", name)
	}
	for _, path := range []string{"/bound", "/tmp"} {
		if name := VolumeName(strings.NewReader(mountInfo), path); name != "" {
			t.Errorf("%s is no volume, but gave %q", path, name)
		}
	}
	denied := DataAdvice("/data", fmt.Errorf("open /data/settings.json: %w", fs.ErrPermission))
	if !strings.Contains(denied.Error(), "chown -R "+strconv.Itoa(os.Getuid())) || !errors.Is(denied, fs.ErrPermission) {
		t.Fatalf("advice for a data folder Scan Box may not write: %v", denied)
	}
	other := errors.New("disk on fire")
	if DataAdvice("/data", other) != other {
		t.Fatal("other problems should pass as they are")
	}
}

func TestChecksShowLinesOnlyWhenMissing(t *testing.T) {
	complete := Facts{ScannerName: "CANON CanoScan LiDE 400", TokenSource: FromFile, PasswordSource: FromFile, DataIsVolume: true}
	checks := Checks(complete)
	if MissingCount(checks) != 0 {
		t.Fatalf("complete set-up reports missing items: %+v", checks)
	}
	for _, check := range checks {
		if check.Snippet != "" {
			t.Fatalf("%s shows lines although it is fine", check.Title)
		}
	}
	checks = Checks(Facts{TokenSource: FromVariable, TokenProblem: "Paperless refused the token"})
	if MissingCount(checks) != 4 {
		t.Fatalf("expected four missing items, got %+v", checks)
	}
	if !strings.Contains(checks[2].Snippet, "SCANBOX_PASSWORD_FILE: /run/secrets/scanbox_password") {
		t.Fatalf("password lines: %q", checks[2].Snippet)
	}
	if checks[1].Snippet != "" || !strings.HasPrefix(checks[1].Help, "Paperless refused the token") {
		t.Fatalf("a refused token needs advice, not lines: %+v", checks[1])
	}
}

func TestATokenThatCantViewTasksSaysWhatToGrant(t *testing.T) {
	token := tokenCheck(Facts{TokenSource: FromFile, TasksDenied: true})
	if token.OK || token.Detail != "Can't view tasks" || !strings.Contains(token.Help, "View for PaperlessTask") || token.Snippet != "" {
		t.Fatalf("a token whose user can't view tasks: %+v", token)
	}
	if missing := tokenCheck(Facts{}); !strings.Contains(missing.Help, "add documents and view tasks") {
		t.Fatalf("a missing token should say what its user needs: %q", missing.Help)
	}
}

func TestFullComposeHasEveryPiece(t *testing.T) {
	compose := FullCompose()
	for _, piece := range []string{"image: ${SCANBOX_IMAGE:-ghcr.io/aiulian25/scanbox:0}\n", "env_file: .env\n", `- "${SCANBOX_ADDRESS:?`, `}:28761:28761"`, "- /dev/bus/usb:/dev/bus/usb:ro", `"c 189:* rw"`, "scanbox-data:/data", "PAPERLESS_TOKEN_FILE", "SCANBOX_PASSWORD_FILE", "read_only: true", "mem_limit: 768m", "pids_limit: 128", "GOMEMLIMIT: 640MiB", "driver: json-file", `max-size: "10m"`, `max-file: "3"`, "external: true", "name: scanbox_scanbox-data"} {
		if !strings.Contains(compose, piece) {
			t.Errorf("full compose lacks %q", piece)
		}
	}
}

// The scans live in a volume Compose never deletes, not even with "docker compose down -v".
func TestTheDataVolumeIsOneComposeNeverDeletes(t *testing.T) {
	data := dataCheck(Facts{})
	for name, lines := range map[string]string{"the data check": data.Snippet, "the full compose file": FullCompose()} {
		if !strings.Contains(lines, "scanbox-data:\n    external: true\n    name: scanbox_scanbox-data") {
			t.Errorf("%s should declare the volume external, by its name:\n%s", name, lines)
		}
	}
	if !strings.Contains(data.Help, "docker volume create scanbox_scanbox-data") {
		t.Errorf("the data check should say how to make the volume: %q", data.Help)
	}
}

func TestUSBDevicesAreMountedNotCopied(t *testing.T) {
	// devices: copies the device files present at the start, so a scanner plugged in later
	// has none; mknod (the m) isn't needed with the host's own files mounted.
	scanner := scannerCheck(Facts{})
	for name, lines := range map[string]string{"the scanner check": scanner.Snippet, "the full compose file": FullCompose()} {
		if strings.Contains(lines, "devices:") || strings.Contains(lines, "rmw") || !strings.Contains(lines, "/dev/bus/usb:/dev/bus/usb:ro") {
			t.Errorf("%s should mount /dev/bus/usb read-only, with the rule c 189:* rw:\n%s", name, lines)
		}
		if !strings.Contains(lines, "group_add:\n      - \"${SCANNER_GID:?") {
			t.Errorf("%s should give user 10001 the scanner's group, and no other:\n%s", name, lines)
		}
	}
}
