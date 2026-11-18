package granola

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cacheState() map[string]any {
	return map[string]any{
		"documents": map[string]any{
			"doc-1": map[string]any{
				"id": "doc-1", "title": "Mine", "user_id": "user-1",
				"created_at": "2026-11-17T15:30:00Z", "updated_at": "2026-11-17T16:45:00Z",
				"notes_markdown": "my typed notes",
			},
			"doc-2": map[string]any{
				"id": "doc-2", "title": "Shared with me", "user_id": "user-2",
				"created_at": "2026-11-16T10:00:00Z", "updated_at": "2026-11-16T11:00:00Z",
			},
		},
		"transcripts": map[string]any{
			"doc-1": []any{
				map[string]any{"text": "hello", "source": "microphone", "start_timestamp": "2026-11-17T15:30:01Z"},
				map[string]any{"text": "hi there", "source": "system", "start_timestamp": "2026-11-17T15:30:04Z"},
			},
		},
	}
}

func TestCacheV3StringEncoded(t *testing.T) {
	state := cacheState()
	state["documentPanels"] = map[string]any{
		"doc-1": map[string]any{"panel-1": map[string]any{"title": "Summary", "content": pmDoc("the summary")}},
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cache-v3.json"), mustJSON(t, map[string]any{
		"cache": mustJSONString(t, map[string]any{"state": state, "version": 3}),
	}))

	cache, err := LoadCache(filepath.Join(dir, "cache-v3.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cache.Version != 3 || cache.Encoding != "string" || len(cache.Documents) != 2 || len(cache.Panels) != 1 || len(cache.Transcripts) != 1 {
		t.Errorf("unexpected outline: version %d, encoding %q, %d documents, %d panels, %d transcripts",
			cache.Version, cache.Encoding, len(cache.Documents), len(cache.Panels), len(cache.Transcripts))
	}

	src := &CacheSource{Dir: dir}
	notes, err := src.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 {
		t.Fatalf("got %d notes, want 2", len(notes))
	}
	n := notes[0]
	if n.ID != "doc-1" || !n.Hydrated || n.NotesMarkdown != "my typed notes" {
		t.Errorf("unexpected note: %+v", n)
	}
	if len(n.Panels) != 1 || n.Panels[0].Markdown != "the summary" {
		t.Errorf("panels = %+v", n.Panels)
	}
	if len(n.Transcript) != 2 || n.Transcript[1].Text != "hi there" {
		t.Errorf("transcript = %+v", n.Transcript)
	}
}

func TestCacheV4Object(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cache-v6.json"), mustJSON(t, map[string]any{
		"cache": map[string]any{"state": cacheState(), "version": 6},
	}))
	cache, err := LoadCache(filepath.Join(dir, "cache-v6.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cache.Version != 6 || cache.Encoding != "object" || len(cache.Documents) != 2 || len(cache.Panels) != 0 {
		t.Errorf("unexpected outline: %+v", cache)
	}
	notes, err := (&CacheSource{Dir: dir}).List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 || len(notes[0].Panels) != 0 || len(notes[0].Transcript) != 2 {
		t.Errorf("unexpected notes: %+v", notes)
	}
}

func TestCacheOwnerFilter(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cache-v4.json"), mustJSON(t, map[string]any{
		"cache": map[string]any{"state": cacheState(), "version": 4},
	}))
	titles := func(userID string) string {
		notes, err := (&CacheSource{Dir: dir, UserID: userID}).List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, n := range notes {
			out = append(out, n.Title)
		}
		return strings.Join(out, ",")
	}
	if got := titles("user-1"); got != "Mine" {
		t.Errorf("with a matching user id got %q, want only the user's own note", got)
	}
	if got := titles("someone-unknown"); got != "Mine,Shared with me" {
		t.Errorf("with a user id that matches nothing got %q, want everything", got)
	}
	if got := titles(""); got != "Mine,Shared with me" {
		t.Errorf("without a user id got %q, want everything", got)
	}
}

func TestNewestCache(t *testing.T) {
	body := mustJSON(t, map[string]any{"cache": map[string]any{"state": cacheState(), "version": 6}})

	t.Run("highest version is used", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "cache-v3.json"), []byte(`{"cache":"{}"}`))
		writeFile(t, filepath.Join(dir, "cache-v10.json"), body)
		writeFile(t, filepath.Join(dir, "cache-v6.json"), body)
		writeFile(t, filepath.Join(dir, "cache-v6.json.bak"), body)
		file, err := NewestCache(dir)
		if err != nil || file.Version != 10 || filepath.Base(file.Path) != "cache-v10.json" {
			t.Errorf("got %+v, %v", file, err)
		}
		all, err := FindCaches(dir)
		if err != nil || len(all) != 3 {
			t.Errorf("FindCaches found %d files, %v; want 3", len(all), err)
		}
	})
	t.Run("an encrypted newest version is not replaced by an older plaintext one", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "cache-v3.json"), body)
		writeFile(t, filepath.Join(dir, "cache-v6.json.enc"), []byte{1, 2, 3})
		if _, err := NewestCache(dir); !errors.Is(err, ErrEncrypted) {
			t.Errorf("error = %v, want ErrEncrypted", err)
		}
		if _, err := (&CacheSource{Dir: dir}).List(context.Background()); !errors.Is(err, ErrEncrypted) {
			t.Errorf("List error = %v, want ErrEncrypted", err)
		}
	})
	t.Run("stale plaintext beside newer encrypted copy", func(t *testing.T) {
		dir := t.TempDir()
		plain := filepath.Join(dir, "cache-v6.json")
		writeFile(t, plain, body)
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(plain, old, old); err != nil {
			t.Fatal(err)
		}
		writeFile(t, plain+".enc", []byte{1, 2, 3})
		if _, err := NewestCache(dir); !errors.Is(err, ErrEncrypted) {
			t.Errorf("error = %v, want ErrEncrypted", err)
		}
	})
	t.Run("none", func(t *testing.T) {
		if _, err := NewestCache(t.TempDir()); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error = %v, want not-exist", err)
		}
		if _, err := NewestCache(filepath.Join(t.TempDir(), "absent")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("error for a missing folder = %v, want not-exist", err)
		}
	})
}

func TestCacheShapeErrors(t *testing.T) {
	cases := map[string]string{
		"not json":           `{"cache": `,
		"cache is a number":  `{"cache": 5}`,
		"no documents":       `{"cache": {"state": {"people": {}, "secretKeyName": "secret value"}, "version": 9}}`,
		"documents an array": `{"cache": {"state": {"documents": []}}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "cache-v9.json")
			writeFile(t, path, []byte(body))
			_, err := LoadCache(path)
			var shape *ShapeError
			if !errors.As(err, &shape) {
				t.Fatalf("error = %v, want a ShapeError", err)
			}
			if strings.Contains(err.Error(), "secret value") {
				t.Errorf("error reveals a value: %v", err)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "cache-v9.json")
	writeFile(t, path, []byte(`{"cache": {"state": {"people": {}, "meetingsMetadata": {}}}}`))
	if _, err := LoadCache(path); err == nil || !strings.Contains(err.Error(), "meetingsMetadata, people") {
		t.Errorf("error should list the keys that were found: %v", err)
	}
}

func TestCacheSkipsUnreadableDocuments(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "cache-v6.json"), []byte(`{"cache":{"state":{"documents":{
		"good": {"title": "ok", "notes_markdown": "x"},
		"bad": "not an object"
	}}}}`))
	src := &CacheSource{Dir: dir}
	notes, err := src.List(context.Background())
	if err != nil || len(notes) != 1 || notes[0].ID != "good" {
		t.Fatalf("got %+v, %v", notes, err)
	}
	if len(src.Notices()) != 1 || !strings.Contains(src.Notices()[0], "1 documents") {
		t.Errorf("notices = %q", src.Notices())
	}

	writeFile(t, filepath.Join(dir, "cache-v6.json"), []byte(`{"cache":{"state":{"documents":{"a": 1, "b": 2}}}}`))
	var shape *ShapeError
	if _, err := (&CacheSource{Dir: dir}).List(context.Background()); !errors.As(err, &shape) {
		t.Errorf("error = %v, want a ShapeError when nothing can be read", err)
	}
}
