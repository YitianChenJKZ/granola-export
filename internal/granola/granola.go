// Package granola reads notes from the Granola Mac app, either through the
// app's signed-in session or from its on-disk cache. It only ever reads
// Granola's files.
package granola

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	SourceApp   = "app"
	SourceCache = "cache"
)

// ErrEncrypted means Granola stores the file encrypted behind the macOS
// Keychain, which this tool does not unlock.
var ErrEncrypted = errors.New("the file is encrypted by Granola")

// ShapeError reports data that is not laid out the way this tool expects.
// Detail names keys and JSON types only, never values.
type ShapeError struct {
	Where  string
	Detail string
}

func (e *ShapeError) Error() string {
	return fmt.Sprintf("unrecognised data in %s: %s", e.Where, e.Detail)
}

type Note struct {
	ID        string
	Title     string
	CreatedAt time.Time
	UpdatedAt time.Time
	// UpdatedRaw is updated_at exactly as Granola sent it, used to tell
	// whether a note changed since the last sync.
	UpdatedRaw string
	Deleted    bool
	OwnerID    string
	Attendees  []string
	// NotesMarkdown is what the user typed.
	NotesMarkdown string
	// Panels are the AI-generated notes.
	Panels     []Panel
	Transcript []Segment

	// Hydrated is false while Panels and Transcript are still to be fetched.
	Hydrated bool
	// PanelsUnavailable and TranscriptUnavailable are set when Granola
	// answered "not found" for that part rather than returning it empty.
	PanelsUnavailable     bool
	TranscriptUnavailable bool
}

type Panel struct {
	Title    string
	Markdown string
}

type Segment struct {
	Start  time.Time
	Source string
	Text   string
}

// Empty reports whether the note has nothing worth exporting.
func (n *Note) Empty(includeTranscript bool) bool {
	if strings.TrimSpace(n.NotesMarkdown) != "" || len(n.Panels) > 0 {
		return false
	}
	return !includeTranscript || len(n.Transcript) == 0
}

// Source is somewhere notes can be read from.
type Source interface {
	Name() string
	// List returns every note. Notes may come back without panels and
	// transcript (Hydrated false) when those cost a request each.
	List(ctx context.Context) ([]*Note, error)
	// Hydrate fills in the panels and, if asked, the transcript.
	Hydrate(ctx context.Context, n *Note, withTranscript bool) error
	// Notices are things the user should be told after a run.
	Notices() []string
}

// DefaultDir is where the Granola Mac app keeps its data.
func DefaultDir(home string) string {
	return filepath.Join(home, "Library", "Application Support", "Granola")
}

var bundleVersion = regexp.MustCompile(`<key>CFBundleShortVersionString</key>\s*<string>([0-9A-Za-z.\-]+)</string>`)

// DetectAppVersion returns the installed Granola version, or "" if the app
// is not found where it is normally installed.
func DetectAppVersion(home string) string {
	for _, app := range []string{"/Applications/Granola.app", filepath.Join(home, "Applications", "Granola.app")} {
		data, err := os.ReadFile(filepath.Join(app, "Contents", "Info.plist"))
		if err != nil {
			continue
		}
		if m := bundleVersion.FindSubmatch(data); m != nil {
			return string(m[1])
		}
	}
	return ""
}

// plaintextFile checks that path can be read as plain JSON. It returns
// ErrEncrypted when an encrypted sibling (path + ".enc") has replaced it or
// is newer, since the plaintext is then missing or stale.
func plaintextFile(path string) error {
	plain, plainErr := os.Stat(path)
	enc, encErr := os.Stat(path + ".enc")
	if encErr == nil && (plainErr != nil || enc.ModTime().After(plain.ModTime())) {
		return fmt.Errorf("%s: %w", filepath.Base(path), ErrEncrypted)
	}
	return plainErr
}

type fields map[string]json.RawMessage

func (f fields) str(key string) string {
	var s string
	if raw, ok := f[key]; ok && json.Unmarshal(raw, &s) == nil {
		return s
	}
	return ""
}

func (f fields) isSet(key string) bool {
	raw, ok := f[key]
	return ok && strings.TrimSpace(string(raw)) != "null"
}

func (f fields) keys() string {
	return describeKeys(f)
}

func describeKeys[V any](m map[string]V) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	const limit = 25
	if len(keys) > limit {
		return fmt.Sprintf("%s and %d more", strings.Join(keys[:limit], ", "), len(keys)-limit)
	}
	if len(keys) == 0 {
		return "(none)"
	}
	return strings.Join(keys, ", ")
}

// jsonType names the type of a JSON value without revealing it.
func jsonType(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	switch {
	case s == "":
		return "nothing"
	case s == "null":
		return "null"
	case s[0] == '{':
		return "an object"
	case s[0] == '[':
		return "an array"
	case s[0] == '"':
		return "a string"
	case s == "true" || s == "false":
		return "a boolean"
	default:
		return "a number"
	}
}

// nestedObject decodes a JSON object that may be stored directly or as a
// JSON-encoded string, which Granola uses in several places.
func nestedObject(raw json.RawMessage) (fields, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		raw = json.RawMessage(s)
	}
	var f fields
	if json.Unmarshal(raw, &f) != nil || f == nil {
		return nil, false
	}
	return f, true
}

func parseTime(raw json.RawMessage) time.Time {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		for _, layout := range []string{
			time.RFC3339Nano,
			"2006-01-02T15:04:05.999999999",
			"2006-01-02 15:04:05.999999999Z07:00",
			"2006-01-02 15:04:05.999999999",
		} {
			if t, err := time.Parse(layout, s); err == nil {
				return t
			}
		}
		return time.Time{}
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		if n > 1e11 {
			return time.UnixMilli(int64(n)).UTC()
		}
		return time.Unix(int64(n), 0).UTC()
	}
	return time.Time{}
}
