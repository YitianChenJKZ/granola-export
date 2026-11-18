package granola

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func pmDoc(text string) map[string]any {
	return map[string]any{"type": "doc", "content": []any{
		map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": text}}},
	}}
}

func TestParseDocument(t *testing.T) {
	raw := mustJSON(t, map[string]any{
		"id":             "doc-1",
		"title":          "  Budget \n review ",
		"created_at":     "2026-11-17T15:30:00.123Z",
		"updated_at":     "2026-11-17T16:45:00Z",
		"deleted_at":     nil,
		"user_id":        "user-1",
		"notes_markdown": "",
		"notes":          pmDoc("typed by hand"),
		"notes_plain":    "plain fallback",
		"people": map[string]any{
			"creator": map[string]any{"name": "Oat Benson", "email": "oat@example.com"},
			"attendees": []any{
				map[string]any{"email": "OAT@example.com"},
				map[string]any{"email": "raisin@example.com", "details": map[string]any{"person": map[string]any{"name": map[string]any{"fullName": "Raisin Patel"}}}},
				map[string]any{"email": "nameless@example.com"},
			},
		},
	})
	n, err := parseDocument(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	if n.ID != "doc-1" || n.Title != "Budget review" || n.OwnerID != "user-1" || n.Deleted {
		t.Errorf("unexpected basics: %+v", n)
	}
	if want := time.Date(2026, 11, 17, 15, 30, 0, 123000000, time.UTC); !n.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", n.CreatedAt, want)
	}
	if n.UpdatedRaw != "2026-11-17T16:45:00Z" {
		t.Errorf("UpdatedRaw = %q", n.UpdatedRaw)
	}
	if n.NotesMarkdown != "typed by hand" {
		t.Errorf("NotesMarkdown = %q, want the converted ProseMirror notes", n.NotesMarkdown)
	}
	if want := []string{"Oat Benson", "Raisin Patel", "nameless@example.com"}; !reflect.DeepEqual(n.Attendees, want) {
		t.Errorf("Attendees = %q, want %q", n.Attendees, want)
	}
	if n.Hydrated {
		t.Error("a listed document must not count as hydrated")
	}
}

func TestParseDocumentNotesPrecedence(t *testing.T) {
	cases := []struct {
		name string
		doc  map[string]any
		want string
	}{
		{"markdown wins", map[string]any{"notes_markdown": "- md", "notes": pmDoc("pm"), "notes_plain": "plain"}, "- md"},
		{"prosemirror next", map[string]any{"notes": pmDoc("pm"), "notes_plain": "plain"}, "pm"},
		{"plain last", map[string]any{"notes": map[string]any{"type": "doc"}, "notes_plain": "plain"}, "plain"},
		{"unreadable notes fall back to plain", map[string]any{"notes": 42, "notes_plain": "plain"}, "plain"},
		{"nothing", map[string]any{"notes": nil}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.doc["id"] = "d"
			n, err := parseDocument(mustJSON(t, c.doc), "")
			if err != nil {
				t.Fatal(err)
			}
			if n.NotesMarkdown != c.want {
				t.Errorf("NotesMarkdown = %q, want %q", n.NotesMarkdown, c.want)
			}
		})
	}
}

func TestParseDocumentShapes(t *testing.T) {
	if n, err := parseDocument([]byte(`{"title":"x"}`), "from-key"); err != nil || n.ID != "from-key" {
		t.Errorf("fallback id not used: %+v, %v", n, err)
	}
	if n, err := parseDocument([]byte(`{"id":"d","deleted_at":"2026-01-01T00:00:00Z"}`), ""); err != nil || !n.Deleted {
		t.Errorf("deleted_at not recognised: %+v, %v", n, err)
	}
	if n, err := parseDocument([]byte(`{"id":"d","created_at":1763393400000,"people":"odd"}`), ""); err != nil || n.CreatedAt.Year() != 2025 || n.Attendees != nil {
		t.Errorf("numeric timestamp or odd people not tolerated: %+v, %v", n, err)
	}
	for _, raw := range []string{`{"title":"secret title"}`, `"text"`, `[]`, `null`} {
		_, err := parseDocument([]byte(raw), "")
		var shape *ShapeError
		if !errors.As(err, &shape) {
			t.Errorf("parseDocument(%s) error = %v, want a ShapeError", raw, err)
		} else if strings.Contains(err.Error(), "secret") {
			t.Errorf("error reveals content: %v", err)
		}
	}
}

func TestParsePanels(t *testing.T) {
	early := map[string]any{"id": "p1", "title": "Summary", "created_at": "2026-01-01T10:00:00Z", "content": pmDoc("first")}
	late := map[string]any{"id": "p2", "title": "", "created_at": "2026-01-02T10:00:00Z", "content": mustJSONString(t, pmDoc("second"))}
	deleted := map[string]any{"id": "p3", "title": "Gone", "deleted_at": "2026-01-03T00:00:00Z", "content": pmDoc("gone")}
	blank := map[string]any{"id": "p4", "title": "Blank", "content": map[string]any{"type": "doc"}}
	want := []Panel{{"Summary", "first"}, {"Summary", "second"}}

	shapes := map[string]any{
		"array":    []any{late, deleted, early, blank},
		"wrapped":  map[string]any{"panels": []any{late, deleted, early, blank}},
		"by id":    map[string]any{"p2": late, "p3": deleted, "p1": early, "p4": blank},
		"sequence": []any{early, late},
	}
	for name, shape := range shapes {
		t.Run(name, func(t *testing.T) {
			got, err := parsePanels(mustJSON(t, shape))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}

	single, err := parsePanels(mustJSON(t, early))
	if err != nil || len(single) != 1 || single[0].Markdown != "first" {
		t.Errorf("single panel: %+v, %v", single, err)
	}
	text, err := parsePanels([]byte(`[{"title":"Raw","content":"already **markdown**"}]`))
	if err != nil || len(text) != 1 || text[0].Markdown != "already **markdown**" {
		t.Errorf("string content: %+v, %v", text, err)
	}
	for _, raw := range []string{``, `null`, `[]`, `{}`} {
		if got, err := parsePanels([]byte(raw)); err != nil || len(got) != 0 {
			t.Errorf("parsePanels(%q) = %+v, %v; want none", raw, got, err)
		}
	}
	for _, raw := range []string{`"text"`, `{"error":"nope"}`, `[1]`, `[{"content":7}]`} {
		if _, err := parsePanels([]byte(raw)); err == nil {
			t.Errorf("parsePanels(%s) returned no error", raw)
		}
	}
}

func mustJSONString(t *testing.T, v any) string {
	return string(mustJSON(t, v))
}

func TestParseTranscript(t *testing.T) {
	segments := []any{
		map[string]any{"text": " second  part ", "source": "system", "start_timestamp": "2026-01-01T10:00:05Z"},
		map[string]any{"text": "first", "source": "microphone", "start_timestamp": "2026-01-01T10:00:00Z"},
		map[string]any{"text": "   ", "source": "system", "start_timestamp": "2026-01-01T10:00:09Z"},
	}
	for name, shape := range map[string]any{"array": segments, "wrapped": map[string]any{"transcript": segments}} {
		t.Run(name, func(t *testing.T) {
			got, err := parseTranscript(mustJSON(t, shape))
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 2 || got[0].Text != "first" || got[0].Source != "microphone" || got[1].Text != "second part" {
				t.Errorf("got %+v", got)
			}
		})
	}
	if _, err := parseTranscript([]byte(`["just text"]`)); err == nil {
		t.Error("expected an error for segments that are not objects")
	}
}

func jwt(t *testing.T, exp time.Time) string {
	payload := base64.RawURLEncoding.EncodeToString(mustJSON(t, map[string]any{"exp": exp.Unix(), "sub": "user-1"}))
	return "eyJhbGciOiJSUzI1NiJ9." + payload + ".c2lnbmF0dXJl"
}

func TestLoadToken(t *testing.T) {
	obtained := time.Date(2026, 11, 18, 8, 0, 0, 0, time.UTC)
	plainTokens := map[string]any{
		"access_token":  "opaque-access-token",
		"refresh_token": "opaque-refresh-token",
		"expires_in":    21600,
		"obtained_at":   obtained.UnixMilli(),
	}
	jwtExp := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	jwtTokens := map[string]any{"access_token": jwt(t, jwtExp), "expires_in": 21600, "obtained_at": obtained.UnixMilli()}

	cases := []struct {
		name       string
		file       map[string]any
		wantToken  string
		wantExpiry time.Time
		wantUser   string
	}{
		{"string-encoded", map[string]any{
			"workos_tokens": mustJSONString(t, plainTokens),
			"user_info":     mustJSONString(t, map[string]any{"id": "user-1", "email": "oat@example.com"}),
		}, "opaque-access-token", obtained.Add(6 * time.Hour), "user-1"},
		{"object", map[string]any{
			"workos_tokens": plainTokens,
			"user_info":     map[string]any{"id": "user-2"},
		}, "opaque-access-token", obtained.Add(6 * time.Hour), "user-2"},
		{"legacy cognito", map[string]any{
			"cognito_tokens": mustJSONString(t, map[string]any{"access_token": "legacy-token"}),
		}, "legacy-token", time.Time{}, ""},
		{"jwt expiry wins", map[string]any{"workos_tokens": jwtTokens}, jwt(t, jwtExp), jwtExp, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, filepath.Join(dir, "supabase.json"), mustJSON(t, c.file))
			tok, err := LoadToken(dir)
			if err != nil {
				t.Fatal(err)
			}
			if tok.AccessToken != c.wantToken || !tok.ExpiresAt.Equal(c.wantExpiry) || tok.UserID != c.wantUser {
				t.Errorf("got %+v", tok)
			}
		})
	}
}

func TestTokenExpired(t *testing.T) {
	exp := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	tok := &Token{ExpiresAt: exp}
	if tok.Expired(exp.Add(-2 * time.Minute)) {
		t.Error("token reported expired two minutes early")
	}
	if !tok.Expired(exp.Add(-30*time.Second)) || !tok.Expired(exp.Add(time.Hour)) {
		t.Error("token not reported expired at or near its expiry")
	}
	if (&Token{}).Expired(exp) {
		t.Error("a token of unknown expiry must be tried, not treated as expired")
	}
}

func TestLoadTokenFailures(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		_, err := LoadToken(t.TempDir())
		if !os.IsNotExist(err) {
			t.Errorf("error = %v, want not-exist", err)
		}
	})
	t.Run("only encrypted", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "supabase.json.enc"), []byte{1, 2, 3})
		if _, err := LoadToken(dir); !errors.Is(err, ErrEncrypted) {
			t.Errorf("error = %v, want ErrEncrypted", err)
		}
	})
	t.Run("stale plaintext beside newer encrypted", func(t *testing.T) {
		dir := t.TempDir()
		plain := filepath.Join(dir, "supabase.json")
		writeFile(t, plain, []byte(`{"workos_tokens":{"access_token":"stale"}}`))
		writeFile(t, plain+".enc", []byte{1, 2, 3})
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(plain, old, old); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadToken(dir); !errors.Is(err, ErrEncrypted) {
			t.Errorf("error = %v, want ErrEncrypted", err)
		}
	})
	t.Run("plaintext newer than encrypted leftover", func(t *testing.T) {
		dir := t.TempDir()
		plain := filepath.Join(dir, "supabase.json")
		writeFile(t, plain+".enc", []byte{1, 2, 3})
		old := time.Now().Add(-time.Hour)
		if err := os.Chtimes(plain+".enc", old, old); err != nil {
			t.Fatal(err)
		}
		writeFile(t, plain, []byte(`{"workos_tokens":{"access_token":"fresh"}}`))
		if tok, err := LoadToken(dir); err != nil || tok.AccessToken != "fresh" {
			t.Errorf("got %+v, %v", tok, err)
		}
	})
	t.Run("unexpected layout", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "supabase.json"), []byte(`{"session":{"access_token":"hidden-value"},"other":1}`))
		_, err := LoadToken(dir)
		var shape *ShapeError
		if !errors.As(err, &shape) {
			t.Fatalf("error = %v, want a ShapeError", err)
		}
		if !strings.Contains(err.Error(), "other, session") || strings.Contains(err.Error(), "hidden-value") {
			t.Errorf("error should name the keys but no values: %v", err)
		}
	})
	t.Run("not json", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, "supabase.json"), []byte(`not json`))
		var shape *ShapeError
		if _, err := LoadToken(dir); !errors.As(err, &shape) {
			t.Errorf("error = %v, want a ShapeError", err)
		}
	})
}
