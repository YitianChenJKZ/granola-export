package schedule

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// plistValues decodes the top-level <dict> of a plist into key → rendered
// value, failing the test if the XML is not well formed.
func plistValues(t *testing.T, data []byte) map[string]string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(string(data)))
	dec.Strict = true
	values := map[string]string{}
	var key string
	var path []string
	var text strings.Builder
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("plist is not well-formed XML: %v\n%s", err, data)
		}
		switch tok := tok.(type) {
		case xml.StartElement:
			path = append(path, tok.Name.Local)
			text.Reset()
			if len(path) == 3 && (tok.Name.Local == "true" || tok.Name.Local == "false") {
				values[key] = tok.Name.Local
			}
		case xml.CharData:
			text.Write(tok)
		case xml.EndElement:
			name := tok.Name.Local
			switch {
			case len(path) == 3 && name == "key":
				key = text.String()
			case len(path) == 3 && (name == "string" || name == "integer"):
				values[key] = text.String()
			case len(path) == 4 && name != "key":
				values[key] += strings.TrimSpace(text.String()) + ";"
			case len(path) == 4:
				values[key] += strings.TrimSpace(text.String()) + "="
			}
			path = path[:len(path)-1]
			text.Reset()
		}
	}
	return values
}

func TestPlist(t *testing.T) {
	data := Plist(Job{
		Binary:  "/Users/oat & co/bin/granola-export",
		Hour:    7,
		Minute:  5,
		LogPath: "/Users/oat & co/Library/Logs/granola-export.log",
	})
	got := plistValues(t, data)
	want := map[string]string{
		"Label":                 "com.granola-export.sync",
		"ProgramArguments":      "/Users/oat & co/bin/granola-export;sync;",
		"StartCalendarInterval": "Hour=7;Minute=5;",
		"RunAtLoad":             "true",
		"ProcessType":           "Background",
		"StandardOutPath":       "/Users/oat & co/Library/Logs/granola-export.log",
		"StandardErrorPath":     "/Users/oat & co/Library/Logs/granola-export.log",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plist values:\n got %q\nwant %q", got, want)
	}
	if !strings.HasPrefix(string(data), `<?xml version="1.0" encoding="UTF-8"?>`+"\n<!DOCTYPE plist PUBLIC") {
		t.Errorf("plist header missing:\n%s", data)
	}
}

func TestParseTime(t *testing.T) {
	for in, want := range map[string][2]int{"09:00": {9, 0}, "23:59": {23, 59}, "0:05": {0, 5}, " 7:30 ": {7, 30}} {
		h, m, err := ParseTime(in)
		if err != nil || h != want[0] || m != want[1] {
			t.Errorf("ParseTime(%q) = %d, %d, %v", in, h, m, err)
		}
	}
	for _, in := range []string{"", "9", "24:00", "09:60", "9am", "09:00:00"} {
		if _, _, err := ParseTime(in); err == nil {
			t.Errorf("ParseTime(%q) returned no error", in)
		}
	}
}

func TestIsTemporaryPath(t *testing.T) {
	temporary := []string{
		filepath.Join(os.TempDir(), "go-build123", "granola-export"),
		"/tmp/granola-export",
		"/private/tmp/x/granola-export",
		"/var/folders/ab/cd/T/go-build1/b001/exe/granola-export",
		"/private/var/folders/ab/cd/T/AppTranslocation/X/granola-export",
	}
	for _, p := range temporary {
		if !IsTemporaryPath(p) {
			t.Errorf("IsTemporaryPath(%q) = false", p)
		}
	}
	permanent := []string{"/usr/local/bin/granola-export", "/Users/oat/bin/granola-export", "/Users/oat/tmp-tools/granola-export", "/tmpfiles/granola-export"}
	for _, p := range permanent {
		if IsTemporaryPath(p) {
			t.Errorf("IsTemporaryPath(%q) = true", p)
		}
	}
}

type fakeLaunchctl struct {
	calls []string
	fail  map[string]int // command → how many times it fails first
}

func (f *fakeLaunchctl) run(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if f.fail[args[0]] > 0 {
		f.fail[args[0]]--
		return []byte("Bootstrap failed: 5: Input/output error\n"), errors.New("exit status 5")
	}
	return nil, nil
}

func manager(t *testing.T, goos string) (*Manager, *fakeLaunchctl) {
	f := &fakeLaunchctl{fail: map[string]int{}}
	return &Manager{Home: t.TempDir(), UID: 501, GOOS: goos, Run: f.run, Sleep: func(time.Duration) {}}, f
}

func TestInstall(t *testing.T) {
	m, launchctl := manager(t, "darwin")
	job := Job{Binary: "/usr/local/bin/granola-export", Hour: 9, LogPath: m.LogPath()}
	if err := m.Install(job); err != nil {
		t.Fatal(err)
	}
	plist := filepath.Join(m.Home, "Library", "LaunchAgents", "com.granola-export.sync.plist")
	data, err := os.ReadFile(plist)
	if err != nil || string(data) != string(Plist(job)) {
		t.Errorf("plist not written as expected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(m.Home, "Library", "Logs")); err != nil {
		t.Errorf("log folder not created: %v", err)
	}
	want := []string{
		"launchctl bootout gui/501/com.granola-export.sync",
		"launchctl bootstrap gui/501 " + plist,
	}
	if !reflect.DeepEqual(launchctl.calls, want) {
		t.Errorf("launchctl calls:\n got %q\nwant %q", launchctl.calls, want)
	}
}

func TestInstallRetriesAndReportsFailure(t *testing.T) {
	m, launchctl := manager(t, "darwin")
	launchctl.fail["bootout"] = 1 // nothing was loaded before: ignored
	launchctl.fail["bootstrap"] = 2
	if err := m.Install(Job{Binary: "/b", LogPath: m.LogPath()}); err != nil {
		t.Fatalf("Install should succeed on the third attempt: %v", err)
	}
	if len(launchctl.calls) != 4 {
		t.Errorf("launchctl calls = %q", launchctl.calls)
	}

	m, launchctl = manager(t, "darwin")
	launchctl.fail["bootstrap"] = 10
	err := m.Install(Job{Binary: "/b", LogPath: m.LogPath()})
	if err == nil || !strings.Contains(err.Error(), "Input/output error") {
		t.Errorf("error = %v, want launchctl's output", err)
	}
}

func TestUninstallAndStatus(t *testing.T) {
	m, launchctl := manager(t, "darwin")
	if s, err := m.Status(); err != nil || s.Installed {
		t.Errorf("status before install = %+v, %v", s, err)
	}
	if removed, err := m.Uninstall(); err != nil || removed {
		t.Errorf("uninstall without a job = %v, %v", removed, err)
	}
	if err := m.Install(Job{Binary: "/b", LogPath: m.LogPath()}); err != nil {
		t.Fatal(err)
	}
	if s, err := m.Status(); err != nil || !s.Installed || !s.Loaded {
		t.Errorf("status after install = %+v, %v", s, err)
	}
	launchctl.fail["print"] = 1
	if s, _ := m.Status(); !s.Installed || s.Loaded {
		t.Errorf("status when launchd does not know the job = %+v", s)
	}
	launchctl.calls = nil
	removed, err := m.Uninstall()
	if err != nil || !removed {
		t.Errorf("uninstall = %v, %v", removed, err)
	}
	if !reflect.DeepEqual(launchctl.calls, []string{"launchctl bootout gui/501/com.granola-export.sync"}) {
		t.Errorf("launchctl calls = %q", launchctl.calls)
	}
	if _, err := os.Stat(m.PlistPath()); !os.IsNotExist(err) {
		t.Errorf("plist still present: %v", err)
	}
}

func TestOtherSystemsAreRefusedBeforeAnythingIsWritten(t *testing.T) {
	m, launchctl := manager(t, "linux")
	if err := m.Install(Job{Binary: "/b", LogPath: m.LogPath()}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Install error = %v", err)
	}
	if _, err := m.Uninstall(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Uninstall error = %v", err)
	}
	if _, err := m.Status(); !errors.Is(err, ErrUnsupported) {
		t.Errorf("Status error = %v", err)
	}
	entries, _ := os.ReadDir(m.Home)
	if len(entries) != 0 || len(launchctl.calls) != 0 {
		t.Errorf("something was written or run: %d entries, calls %q", len(entries), launchctl.calls)
	}
}
