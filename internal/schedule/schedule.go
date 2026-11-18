// Package schedule registers a launchd LaunchAgent that runs the sync once a
// day for the logged-in user.
package schedule

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Label identifies the job to launchd.
const Label = "com.granola-export.sync"

// ErrUnsupported is returned on systems without launchd.
var ErrUnsupported = errors.New("scheduling is only supported on macOS")

// Job is what launchd is asked to run.
type Job struct {
	// Binary is the absolute path of the granola-export executable.
	Binary string
	Hour   int
	Minute int
	// LogPath receives everything the job prints.
	LogPath string
}

// Plist returns the LaunchAgent definition for the job. launchd runs it at
// the given local time every day, when the Mac wakes if it was asleep at
// that time, and at login (RunAtLoad), which covers days the Mac was off.
func Plist(j Job) []byte {
	var b bytes.Buffer
	str := func(s string) string {
		var e bytes.Buffer
		_ = xml.EscapeText(&e, []byte(s))
		return "<string>" + e.String() + "</string>"
	}
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	` + str(Label) + `
	<key>ProgramArguments</key>
	<array>
		` + str(j.Binary) + `
		<string>sync</string>
	</array>
	<key>StartCalendarInterval</key>
	<dict>
		<key>Hour</key>
		<integer>` + strconv.Itoa(j.Hour) + `</integer>
		<key>Minute</key>
		<integer>` + strconv.Itoa(j.Minute) + `</integer>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	` + str(j.LogPath) + `
	<key>StandardErrorPath</key>
	` + str(j.LogPath) + `
</dict>
</plist>
`)
	return b.Bytes()
}

// ParseTime reads a time of day given as HH:MM (24-hour clock).
func ParseTime(s string) (hour, minute int, err error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, 0, fmt.Errorf("%q is not a time of day; use HH:MM on the 24-hour clock, such as 09:00", s)
	}
	return t.Hour(), t.Minute(), nil
}

// IsTemporaryPath reports whether path lies somewhere macOS clears out, where
// a binary must not be referenced from a job meant to run for months.
func IsTemporaryPath(path string) bool {
	path = filepath.Clean(path)
	roots := []string{os.TempDir(), "/tmp", "/private/tmp", "/var/tmp", "/private/var/tmp", "/var/folders", "/private/var/folders"}
	for _, root := range roots {
		root = filepath.Clean(root)
		if path == root || strings.HasPrefix(path, root+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Manager installs and removes the job for one user.
type Manager struct {
	Home string
	UID  int
	GOOS string
	// Run executes a command and returns its combined output.
	Run func(name string, args ...string) ([]byte, error)
	// Sleep waits between attempts to load the job.
	Sleep func(time.Duration)
}

func (m *Manager) PlistPath() string {
	return filepath.Join(m.Home, "Library", "LaunchAgents", Label+".plist")
}

func (m *Manager) LogPath() string {
	return filepath.Join(m.Home, "Library", "Logs", "granola-export.log")
}

func (m *Manager) domain() string  { return "gui/" + strconv.Itoa(m.UID) }
func (m *Manager) service() string { return m.domain() + "/" + Label }

// Install writes the LaunchAgent and loads it, replacing an earlier one.
// Because of RunAtLoad the first sync starts right away.
func (m *Manager) Install(j Job) error {
	if m.GOOS != "darwin" {
		return ErrUnsupported
	}
	for _, dir := range []string{filepath.Dir(m.PlistPath()), filepath.Dir(j.LogPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	if err := os.WriteFile(m.PlistPath(), Plist(j), 0o644); err != nil {
		return err
	}
	// Unload a previous version; this fails harmlessly if there is none.
	_, _ = m.Run("launchctl", "bootout", m.service())

	var out []byte
	var err error
	// launchd may still be tearing down the old job, in which case loading
	// fails for a moment.
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			m.Sleep(time.Second)
		}
		if out, err = m.Run("launchctl", "bootstrap", m.domain(), m.PlistPath()); err == nil {
			return nil
		}
	}
	return fmt.Errorf("launchctl could not load %s: %v: %s", m.PlistPath(), err, strings.TrimSpace(string(out)))
}

// Uninstall unloads the job and deletes its LaunchAgent file. It reports
// whether there was one.
func (m *Manager) Uninstall() (removed bool, err error) {
	if m.GOOS != "darwin" {
		return false, ErrUnsupported
	}
	_, _ = m.Run("launchctl", "bootout", m.service())
	err = os.Remove(m.PlistPath())
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

type Status struct {
	// Installed means the LaunchAgent file exists.
	Installed bool
	// Loaded means launchd currently knows the job.
	Loaded bool
}

func (m *Manager) Status() (Status, error) {
	if m.GOOS != "darwin" {
		return Status{}, ErrUnsupported
	}
	var s Status
	if _, err := os.Stat(m.PlistPath()); err == nil {
		s.Installed = true
	}
	_, err := m.Run("launchctl", "print", m.service())
	s.Loaded = err == nil
	return s, nil
}
