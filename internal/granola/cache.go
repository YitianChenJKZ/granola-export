package granola

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

var cacheFileName = regexp.MustCompile(`^cache-v(\d+)\.json(\.enc)?$`)

// CacheFile describes one cache-vN.json in the Granola data folder.
type CacheFile struct {
	Path    string
	Version int
	Size    int64
	ModTime time.Time
	// Encrypted is true when only an encrypted copy exists or it is newer
	// than the plaintext one.
	Encrypted bool
}

// FindCaches lists the cache files in dir, newest format version first.
func FindCaches(dir string) ([]CacheFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	type pair struct{ plain, enc os.FileInfo }
	byVersion := map[int]*pair{}
	for _, entry := range entries {
		m := cacheFileName.FindStringSubmatch(entry.Name())
		if m == nil || entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		version, _ := strconv.Atoi(m[1])
		p := byVersion[version]
		if p == nil {
			p = &pair{}
			byVersion[version] = p
		}
		if m[2] == "" {
			p.plain = info
		} else {
			p.enc = info
		}
	}
	var out []CacheFile
	for version, p := range byVersion {
		f := CacheFile{Version: version}
		f.Encrypted = p.enc != nil && (p.plain == nil || p.enc.ModTime().After(p.plain.ModTime()))
		info := p.plain
		if f.Encrypted {
			info = p.enc
		}
		f.Path = filepath.Join(dir, info.Name())
		f.Size = info.Size()
		f.ModTime = info.ModTime()
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version > out[j].Version })
	return out, nil
}

// NewestCache returns the cache with the highest format version. Older
// versions are leftovers the app no longer updates, so they are never used
// in its place.
func NewestCache(dir string) (CacheFile, error) {
	caches, err := FindCaches(dir)
	if err != nil {
		return CacheFile{}, err
	}
	if len(caches) == 0 {
		return CacheFile{}, fmt.Errorf("no cache-v*.json in %s: %w", dir, os.ErrNotExist)
	}
	if caches[0].Encrypted {
		return caches[0], fmt.Errorf("%s: %w", filepath.Base(caches[0].Path), ErrEncrypted)
	}
	return caches[0], nil
}

// Cache is the parsed outline of a cache file. Values are left as raw JSON.
type Cache struct {
	// Version is the format version recorded inside the file, 0 if absent.
	Version int
	// Encoding is "string" when the cache body is a JSON-encoded string
	// (v3) and "object" when it is stored directly (v4 and later).
	Encoding    string
	Documents   map[string]json.RawMessage
	Transcripts map[string]json.RawMessage
	// Panels holds the AI notes per document. Only v3 caches have them.
	Panels map[string]json.RawMessage
}

// LoadCache reads a cache file.
func LoadCache(path string) (*Cache, error) {
	name := filepath.Base(path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var top fields
	if err := json.Unmarshal(data, &top); err != nil || top == nil {
		return nil, &ShapeError{name, "not a JSON object (Granola may have been writing the file; try again)"}
	}

	c := &Cache{Encoding: "object"}
	body := top
	if raw, ok := top["cache"]; ok {
		if jsonType(raw) == "a string" {
			c.Encoding = "string"
		}
		inner, ok := nestedObject(raw)
		if !ok {
			return nil, &ShapeError{name, `"cache" is ` + jsonType(raw) + ", expected an object or a JSON-encoded object"}
		}
		body = inner
	}
	var version float64
	if json.Unmarshal(body["version"], &version) == nil {
		c.Version = int(version)
	}
	state := body
	if raw, ok := body["state"]; ok {
		inner, ok := nestedObject(raw)
		if !ok {
			return nil, &ShapeError{name, `"state" is ` + jsonType(raw) + ", expected an object"}
		}
		state = inner
	}
	raw, ok := state["documents"]
	if !ok {
		return nil, &ShapeError{name, `no "documents" found; keys at that level: ` + state.keys()}
	}
	if err := json.Unmarshal(raw, &c.Documents); err != nil {
		return nil, &ShapeError{name, `"documents" is ` + jsonType(raw) + ", expected an object keyed by document id"}
	}
	// Transcripts and panels are optional, so a different layout there only
	// means the export goes without them.
	_ = json.Unmarshal(state["transcripts"], &c.Transcripts)
	_ = json.Unmarshal(state["documentPanels"], &c.Panels)
	return c, nil
}

// CacheStats counts what a cache holds, for diagnostics.
type CacheStats struct {
	Documents      int
	Unreadable     int
	Deleted        int
	WithTypedNotes int
	WithPanels     int
	WithTranscript int
}

func (c *Cache) Stats() CacheStats {
	s := CacheStats{Documents: len(c.Documents)}
	for id, raw := range c.Documents {
		n, err := parseDocument(raw, id)
		if err != nil {
			s.Unreadable++
			continue
		}
		if n.Deleted {
			s.Deleted++
		}
		if n.NotesMarkdown != "" {
			s.WithTypedNotes++
		}
		if panels, err := parsePanels(c.Panels[id]); err == nil && len(panels) > 0 {
			s.WithPanels++
		}
		if segments, err := parseTranscript(c.Transcripts[id]); err == nil && len(segments) > 0 {
			s.WithTranscript++
		}
	}
	return s
}

// CacheSource reads notes from the cache file the Granola app keeps on disk.
type CacheSource struct {
	Dir string
	// UserID, when known, limits the export to the user's own notes: the
	// cache also holds notes other people shared.
	UserID string

	file       CacheFile
	unreadable int
}

func (s *CacheSource) Name() string { return SourceCache }

// File returns the cache file the last List read.
func (s *CacheSource) File() CacheFile { return s.file }

func (s *CacheSource) List(ctx context.Context) ([]*Note, error) {
	file, err := NewestCache(s.Dir)
	if err != nil {
		return nil, err
	}
	cache, err := LoadCache(file.Path)
	if err != nil {
		return nil, err
	}
	s.file = file
	s.unreadable = 0

	ids := make([]string, 0, len(cache.Documents))
	for id := range cache.Documents {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var notes []*Note
	owned := 0
	for _, id := range ids {
		n, err := parseDocument(cache.Documents[id], id)
		if err == nil {
			n.Panels, err = parsePanels(cache.Panels[id])
		}
		if err == nil {
			n.Transcript, err = parseTranscript(cache.Transcripts[id])
		}
		if err != nil {
			s.unreadable++
			continue
		}
		n.Hydrated = true
		if s.UserID != "" && n.OwnerID == s.UserID {
			owned++
		}
		notes = append(notes, n)
	}
	if len(notes) == 0 && s.unreadable > 0 {
		return nil, &ShapeError{filepath.Base(file.Path), fmt.Sprintf("none of the %d documents could be read", s.unreadable)}
	}
	// Only filter when the user id demonstrably matches how documents
	// record their owner; otherwise a format change would hide every note.
	if owned > 0 {
		mine := notes[:0]
		for _, n := range notes {
			if n.OwnerID == s.UserID {
				mine = append(mine, n)
			}
		}
		notes = mine
	}
	return notes, nil
}

func (s *CacheSource) Hydrate(ctx context.Context, n *Note, withTranscript bool) error {
	n.Hydrated = true
	return nil
}

func (s *CacheSource) Notices() []string {
	if s.unreadable == 0 {
		return nil
	}
	return []string{fmt.Sprintf("%d documents in the cache could not be read and were skipped.", s.unreadable)}
}
