package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

const (
	secretToken   = "eyJhbGciOiJSUzI1NiJ9"
	secretContent = "confidential roadmap detail"
)

type harness struct {
	t          *testing.T
	env        *env
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	granolaDir string
	out        string
	api        *fakeGranola
	commands   []string
}

// fakeGranola stands in for the Granola API.
type fakeGranola struct {
	server   *httptest.Server
	docs     []map[string]any
	panels   map[string][]any
	requests map[string]int
	tokens   map[string]bool
	down     bool
}

func (f *fakeGranola) handle(w http.ResponseWriter, r *http.Request) {
	f.requests[r.URL.Path]++
	f.tokens[r.Header.Get("Authorization")] = true
	if f.down {
		http.Error(w, `{"message":"maintenance"}`, http.StatusBadRequest)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	var response any
	switch r.URL.Path {
	case "/v2/get-documents":
		if offset, _ := body["offset"].(float64); offset > 0 {
			response = map[string]any{"docs": []any{}}
		} else {
			response = map[string]any{"docs": f.docs}
		}
	case "/v1/get-document-panels":
		response = f.panels[body["document_id"].(string)]
		if response == nil {
			response = []any{}
		}
	case "/v1/get-document-transcript":
		response = []any{map[string]any{"text": "spoken words", "source": "system", "start_timestamp": "2026-11-17T15:30:05Z"}}
	default:
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func paragraphDoc(text string) map[string]any {
	return map[string]any{"type": "doc", "content": []any{
		map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": text}}},
	}}
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	h := &harness{
		t:          t,
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
		granolaDir: filepath.Join(root, "Granola"),
		out:        filepath.Join(root, "vault", "Meetings"),
	}
	if err := os.MkdirAll(h.granolaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h.api = &fakeGranola{
		requests: map[string]int{},
		tokens:   map[string]bool{},
		docs: []map[string]any{
			{"id": "doc-budget", "title": "Budget review", "created_at": "2026-11-17T15:30:00Z", "updated_at": "2026-11-17T16:45:00Z",
				"user_id": "user-1", "notes_markdown": "typed: " + secretContent,
				"people": map[string]any{"creator": map[string]any{"name": "Oat Benson", "email": "oat@example.com"}}},
			{"id": "doc-old", "title": "Kickoff", "created_at": "2026-09-01T10:00:00Z", "updated_at": "2026-09-01T11:00:00Z", "user_id": "user-1"},
		},
		panels: map[string][]any{
			"doc-budget": {map[string]any{"title": "Summary", "content": paragraphDoc("AI summary of the budget")}},
			"doc-old":    {map[string]any{"title": "Summary", "content": paragraphDoc("AI summary of the kickoff")}},
		},
	}
	h.api.server = httptest.NewServer(http.HandlerFunc(h.api.handle))
	t.Cleanup(h.api.server.Close)

	h.env = &env{
		stdout:     h.stdout,
		stderr:     h.stderr,
		home:       filepath.Join(root, "home"),
		granolaDir: h.granolaDir,
		configDir:  filepath.Join(root, "config"),
		apiBaseURL: h.api.server.URL,
		appVersion: "7.1.0",
		goos:       "darwin",
		uid:        501,
		loc:        time.UTC,
		now:        func() time.Time { return testNow },
		executable: func() (string, error) { return filepath.Join(root, "bin", "granola-export"), nil },
		// Test folders live under the system temporary folder, so only
		// the Go build cache counts as temporary here.
		isTemporary: func(path string) bool { return strings.Contains(path, "go-build") },
		sleep:       func(time.Duration) {},
		runCommand: func(name string, args ...string) ([]byte, error) {
			h.commands = append(h.commands, name+" "+strings.Join(args, " "))
			return nil, nil
		},
	}
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "granola-export"), []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) run(args ...string) int {
	h.stdout.Reset()
	h.stderr.Reset()
	return run(context.Background(), h.env, args)
}

func (h *harness) writeLogin(expires time.Time) {
	payload, _ := json.Marshal(map[string]any{"exp": expires.Unix()})
	token := secretToken + "." + base64.RawURLEncoding.EncodeToString(payload) + ".c2ln"
	tokens, _ := json.Marshal(map[string]any{"access_token": token, "refresh_token": "refresh-" + secretToken})
	user, _ := json.Marshal(map[string]any{"id": "user-1", "email": "oat@example.com"})
	data, _ := json.Marshal(map[string]any{"workos_tokens": string(tokens), "user_info": string(user)})
	if err := os.WriteFile(filepath.Join(h.granolaDir, "supabase.json"), data, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) writeCache() {
	documents := map[string]any{}
	for _, doc := range h.api.docs {
		documents[doc["id"].(string)] = doc
	}
	data, _ := json.Marshal(map[string]any{"cache": map[string]any{"version": 6, "state": map[string]any{
		"documents": documents,
		"transcripts": map[string]any{"doc-budget": []any{
			map[string]any{"text": "cached words", "source": "microphone", "start_timestamp": "2026-11-17T15:30:05Z"},
		}},
	}}})
	if err := os.WriteFile(filepath.Join(h.granolaDir, "cache-v6.json"), data, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) files() []string {
	var out []string
	_ = filepath.WalkDir(h.out, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.Contains(path, ".granola-export") {
			rel, _ := filepath.Rel(h.out, path)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func (h *harness) read(name string) string {
	data, err := os.ReadFile(filepath.Join(h.out, name))
	if err != nil {
		h.t.Fatal(err)
	}
	return string(data)
}

func (h *harness) expectOutput(wants ...string) {
	h.t.Helper()
	all := h.stdout.String() + h.stderr.String()
	for _, want := range wants {
		if !strings.Contains(all, want) {
			h.t.Errorf("output lacks %q:\n%s", want, all)
		}
	}
}

func TestSyncWithGranolaLogin(t *testing.T) {
	h := newHarness(t)
	h.writeLogin(testNow.Add(5 * time.Hour))

	if code := h.run("sync", "--out", h.out); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, h.stdout, h.stderr)
	}
	h.expectOutput("Source: Granola's login, 2 notes", "Created (2):", "2026-11-17 Budget review.md", "2026-09-01 Kickoff.md", "Unchanged: 0")
	if got := strings.Join(h.files(), "|"); got != "2026-09-01 Kickoff.md|2026-11-17 Budget review.md" {
		t.Fatalf("files = %s", got)
	}
	want := `---
title: "Budget review"
date: 2026-11-17
created: 2026-11-17T15:30:00
updated: 2026-11-17T16:45:00
granola_id: "doc-budget"
granola_url: https://notes.granola.ai/d/doc-budget
attendees:
  - "Oat Benson"
tags:
  - granola
---

## Summary

AI summary of the budget

## My notes

typed: confidential roadmap detail

> [!quote]- Transcript
> **Them** 15:30: spoken words
`
	if got := h.read("2026-11-17 Budget review.md"); got != want {
		t.Errorf("file content:\n%s\nwant:\n%s", got, want)
	}
	if len(h.api.tokens) != 1 {
		t.Errorf("requests carried %d different Authorization values", len(h.api.tokens))
	}

	// A second run fetches details only for the note from the last week
	// and writes nothing.
	before := h.api.requests["/v1/get-document-panels"]
	if code := h.run("sync", "--out", h.out); code != 0 {
		t.Fatalf("second run exit %d\n%s%s", code, h.stdout, h.stderr)
	}
	h.expectOutput("Unchanged: 2")
	if strings.Contains(h.stdout.String(), "Created") || strings.Contains(h.stdout.String(), "Updated") {
		t.Errorf("second run wrote files:\n%s", h.stdout)
	}
	if got := h.api.requests["/v1/get-document-panels"] - before; got != 1 {
		t.Errorf("second run made %d panel requests, want 1", got)
	}
}

func TestSyncFlags(t *testing.T) {
	h := newHarness(t)
	h.writeLogin(testNow.Add(5 * time.Hour))

	if code := h.run("sync", "--out", h.out, "--dry-run"); code != 0 {
		t.Fatalf("dry run exit %d\n%s%s", code, h.stdout, h.stderr)
	}
	h.expectOutput("dry run", "Would create (2):")
	if _, err := os.Stat(h.out); !os.IsNotExist(err) {
		t.Errorf("dry run created the output folder")
	}

	transcriptRequests := h.api.requests["/v1/get-document-transcript"]
	if code := h.run("sync", "--out", h.out, "--since", "2026-11-01", "--no-transcript"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, h.stdout, h.stderr)
	}
	if got := strings.Join(h.files(), "|"); got != "2026-11-17 Budget review.md" {
		t.Errorf("files with --since = %s", got)
	}
	if strings.Contains(h.read("2026-11-17 Budget review.md"), "Transcript") {
		t.Error("--no-transcript still exported the transcript")
	}
	if h.api.requests["/v1/get-document-transcript"] != transcriptRequests {
		t.Error("--no-transcript still requested transcripts")
	}

	for _, args := range [][]string{
		{"sync"},
		{"sync", "--out", h.out, "--source", "cloud"},
		{"sync", "--out", h.out, "--since", "yesterday"},
		{"sync", "--out", h.out, "extra"},
		{"sync", "--nonsense"},
		{"frobnicate"},
		{},
	} {
		if code := h.run(args...); code != 2 {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
}

func TestSyncFallsBackToTheCache(t *testing.T) {
	t.Run("expired login", func(t *testing.T) {
		h := newHarness(t)
		h.writeLogin(testNow.Add(-time.Hour))
		h.writeCache()
		if code := h.run("sync", "--out", h.out); code != 0 {
			t.Fatalf("exit %d\n%s%s", code, h.stdout, h.stderr)
		}
		h.expectOutput("Granola's login expired at 2026-11-18 08:00", "Falling back", "Source: Granola's local cache (cache-v6.json", "Created (1):", "Skipped, nothing to export: 1")
		if len(h.api.requests) != 0 {
			t.Errorf("an expired login was still sent to Granola: %v", h.api.requests)
		}
		got := h.read("2026-11-17 Budget review.md")
		if strings.Contains(got, "## Summary") || !strings.Contains(got, "cached words") || !strings.Contains(got, "typed: ") {
			t.Errorf("cache export:\n%s", got)
		}

		// Once the login works again the file gains the AI notes, and the
		// note that was empty in the cache is exported.
		h.writeLogin(testNow.Add(5 * time.Hour))
		if code := h.run("sync", "--out", h.out); code != 0 {
			t.Fatalf("exit %d\n%s%s", code, h.stdout, h.stderr)
		}
		h.expectOutput("Updated (1):", "Created (1):")
		if got := h.read("2026-11-17 Budget review.md"); !strings.Contains(got, "AI summary of the budget") {
			t.Errorf("file not completed:\n%s", got)
		}
	})
	t.Run("Granola unreachable", func(t *testing.T) {
		h := newHarness(t)
		h.writeLogin(testNow.Add(5 * time.Hour))
		h.writeCache()
		h.api.down = true
		if code := h.run("sync", "--out", h.out); code != 0 {
			t.Fatalf("exit %d\n%s%s", code, h.stdout, h.stderr)
		}
		h.expectOutput("cannot list notes with Granola's login", "HTTP 400: maintenance", "Falling back", "Created (1):")
	})
	t.Run("no fallback when the app source is demanded", func(t *testing.T) {
		h := newHarness(t)
		h.writeLogin(testNow.Add(-time.Hour))
		h.writeCache()
		if code := h.run("sync", "--out", h.out, "--source", "app"); code != 1 {
			t.Fatalf("exit %d, want 1\n%s%s", code, h.stdout, h.stderr)
		}
		h.expectOutput("expired")
		if len(h.files()) != 0 {
			t.Errorf("files were written: %q", h.files())
		}
	})
	t.Run("cache only", func(t *testing.T) {
		h := newHarness(t)
		h.writeLogin(testNow.Add(5 * time.Hour))
		h.writeCache()
		if code := h.run("sync", "--out", h.out, "--source", "cache"); code != 0 {
			t.Fatalf("exit %d\n%s%s", code, h.stdout, h.stderr)
		}
		if len(h.api.requests) != 0 {
			t.Errorf("--source cache contacted Granola: %v", h.api.requests)
		}
	})
}

func TestSyncWithNothingToReadFrom(t *testing.T) {
	t.Run("no Granola data", func(t *testing.T) {
		h := newHarness(t)
		if code := h.run("sync", "--out", h.out); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		h.expectOutput("supabase.json is missing", "no Granola cache file")
	})
	t.Run("encrypted storage", func(t *testing.T) {
		h := newHarness(t)
		for _, name := range []string{"supabase.json.enc", "cache-v6.json.enc"} {
			if err := os.WriteFile(filepath.Join(h.granolaDir, name), []byte{1, 2, 3}, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if code := h.run("sync", "--out", h.out); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		h.expectOutput("stores its login encrypted", "stores its cache encrypted")
		if _, err := os.Stat(h.out); !os.IsNotExist(err) {
			t.Error("the output folder was created although nothing could be read")
		}
	})
}

func TestSyncReportsEditedFilesAndFailures(t *testing.T) {
	h := newHarness(t)
	h.writeLogin(testNow.Add(5 * time.Hour))
	if code := h.run("sync", "--out", h.out); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, h.stdout, h.stderr)
	}
	path := filepath.Join(h.out, "2026-11-17 Budget review.md")
	edited := h.read("2026-11-17 Budget review.md") + "\nmy addition\n"
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	h.api.docs[0]["updated_at"] = "2026-11-18T08:00:00Z"
	h.api.panels["doc-budget"] = []any{map[string]any{"title": "Summary", "content": paragraphDoc("regenerated summary")}}

	if code := h.run("sync", "--out", h.out); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, h.stdout, h.stderr)
	}
	h.expectOutput("Left alone: edited after export", "2026-11-17 Budget review.md", "--overwrite-edited")
	if h.read("2026-11-17 Budget review.md") != edited {
		t.Error("the edited file was overwritten")
	}

	if code := h.run("sync", "--out", h.out, "--overwrite-edited"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, h.stdout, h.stderr)
	}
	if got := h.read("2026-11-17 Budget review.md"); !strings.Contains(got, "regenerated summary") || strings.Contains(got, "my addition") {
		t.Errorf("--overwrite-edited did not replace the file:\n%s", got)
	}

	// A note whose details cannot be read is reported and fails the run,
	// without stopping the others.
	h.api.docs[0]["updated_at"] = "2026-11-18T08:30:00Z"
	h.api.panels["doc-budget"] = []any{"not a panel"}
	if code := h.run("sync", "--out", h.out); code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, h.stdout, h.stderr)
	}
	h.expectOutput("Failed (1):", "Budget review (doc-budget)", "Unchanged: 1")
}

func TestScheduleInstallStatusUninstall(t *testing.T) {
	h := newHarness(t)
	h.writeLogin(testNow.Add(5 * time.Hour))

	if code := h.run("schedule", "install", "--out", h.out, "--at", "7:30", "--no-transcript"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, h.stdout, h.stderr)
	}
	plistPath := filepath.Join(h.env.home, "Library", "LaunchAgents", "com.granola-export.sync.plist")
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	binary, _ := h.env.executable()
	binary, _ = filepath.EvalSymlinks(binary)
	for _, want := range []string{"<string>" + binary + "</string>", "<string>sync</string>", "<integer>7</integer>", "<integer>30</integer>", "<key>RunAtLoad</key>"} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist lacks %s:\n%s", want, plist)
		}
	}
	if got := strings.Join(h.commands, "\n"); got != "launchctl bootout gui/501/com.granola-export.sync\nlaunchctl bootstrap gui/501 "+plistPath {
		t.Errorf("commands run:\n%s", got)
	}
	h.expectOutput("Daily sync installed", "every day at 07:30", h.out)
	if info, err := os.Stat(h.out); err != nil || !info.IsDir() {
		t.Errorf("output folder not created: %v", err)
	}

	// The job runs plain "sync", which takes its settings from the install.
	if code := h.run("sync"); code != 0 {
		t.Fatalf("sync exit %d\n%s%s", code, h.stdout, h.stderr)
	}
	if len(h.files()) != 2 || strings.Contains(h.read("2026-11-17 Budget review.md"), "Transcript") {
		t.Errorf("scheduled sync did not honour the saved settings: %q", h.files())
	}

	if code := h.run("schedule", "status"); code != 0 {
		t.Fatalf("status exit %d", code)
	}
	h.expectOutput("installed and loaded", "every day at 07:30", h.out, "Last sync: 2026-11-18 09:00 from Granola's login: 2 created")

	if code := h.run("schedule", "uninstall"); code != 0 {
		t.Fatalf("uninstall exit %d", code)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Error("plist not removed")
	}
	if len(h.files()) != 2 {
		t.Error("uninstall removed exported notes")
	}
	if code := h.run("schedule", "uninstall"); code != 0 {
		t.Fatalf("second uninstall exit %d", code)
	}
	h.expectOutput("No daily sync was installed")
}

func TestScheduleInstallRefusals(t *testing.T) {
	t.Run("temporary binary", func(t *testing.T) {
		h := newHarness(t)
		binary := filepath.Join(t.TempDir(), "go-build42", "b001", "exe", "granola-export")
		if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(binary, nil, 0o755); err != nil {
			t.Fatal(err)
		}
		h.env.executable = func() (string, error) { return binary, nil }
		if code := h.run("schedule", "install", "--out", h.out); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		h.expectOutput("temporary location")
		if len(h.commands) != 0 {
			t.Errorf("launchctl was run: %q", h.commands)
		}
		if _, err := os.Stat(h.env.configPath()); !os.IsNotExist(err) {
			t.Error("settings were saved although the install was refused")
		}
	})
	t.Run("not macOS", func(t *testing.T) {
		h := newHarness(t)
		h.env.goos = "linux"
		for _, args := range [][]string{{"schedule", "install", "--out", h.out}, {"schedule", "uninstall"}, {"schedule", "status"}} {
			if code := h.run(args...); code != 1 {
				t.Errorf("%q: exit %d, want 1", args, code)
			}
			h.expectOutput("only supported on macOS")
		}
		if _, err := os.Stat(h.out); !os.IsNotExist(err) {
			t.Error("the output folder was created")
		}
	})
	t.Run("launchctl fails", func(t *testing.T) {
		h := newHarness(t)
		h.env.runCommand = func(name string, args ...string) ([]byte, error) {
			return []byte("Bootstrap failed: 125: Domain does not support specified action"), errors.New("exit status 125")
		}
		if code := h.run("schedule", "install", "--out", h.out); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		h.expectOutput("Domain does not support specified action")
	})
	t.Run("usage", func(t *testing.T) {
		h := newHarness(t)
		for _, args := range [][]string{{"schedule"}, {"schedule", "install"}, {"schedule", "install", "--out", h.out, "--at", "25:00"}, {"schedule", "pause"}} {
			if code := h.run(args...); code != 2 {
				t.Errorf("%q: exit %d, want 2", args, code)
			}
		}
	})
}

func TestDoctor(t *testing.T) {
	h := newHarness(t)
	h.writeLogin(testNow.Add(5*time.Hour + 12*time.Minute))
	h.writeCache()
	if err := os.WriteFile(filepath.Join(h.granolaDir, "cache-v3.json"), []byte(`{"cache":"{}"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := h.run("doctor"); code != 0 {
		t.Fatalf("exit %d\n%s%s", code, h.stdout, h.stderr)
	}
	h.expectOutput(
		"Granola app: version 7.1.0",
		"present; valid until 2026-11-18 14:12 (5h12m0s from now)",
		"cache-v6.json: 0.0 MB",
		"cache-v3.json: 0.0 MB", "[older format, ignored]",
		"layout recognised: format version 6, stored directly",
		"documents:           2",
		"with typed notes:  1",
		"with AI notes:     0",
		"with a transcript: 1",
		"A sync would use Granola's login, with the cache as fallback.",
	)
	out := h.stdout.String() + h.stderr.String()
	for _, secret := range []string{secretToken, secretContent, "Budget review", "oat@example.com", "user-1"} {
		if strings.Contains(out, secret) {
			t.Errorf("doctor output reveals %q:\n%s", secret, out)
		}
	}
	if len(h.api.requests) != 0 {
		t.Errorf("doctor contacted Granola: %v", h.api.requests)
	}
}

func TestDoctorProblems(t *testing.T) {
	t.Run("no data folder", func(t *testing.T) {
		h := newHarness(t)
		if code := h.run("doctor", "--granola-dir", filepath.Join(h.granolaDir, "absent")); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		h.expectOutput("NOT FOUND")
	})
	t.Run("expired login and unrecognised cache", func(t *testing.T) {
		h := newHarness(t)
		h.env.appVersion = ""
		h.writeLogin(testNow.Add(-time.Hour))
		body := `{"cache":{"version":9,"state":{"notesById":{"x":{"text":"` + secretContent + `"}},"people":{}}}}`
		if err := os.WriteFile(filepath.Join(h.granolaDir, "cache-v9.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if code := h.run("doctor"); code != 1 {
			t.Fatalf("exit %d, want 1\n%s", code, h.stdout)
		}
		h.expectOutput("Granola app: not found", "EXPIRED at 2026-11-18 08:00", "layout not recognised", "notesById, people", "Nothing a sync could read from")
		if strings.Contains(h.stdout.String(), secretContent) {
			t.Errorf("doctor output reveals note content:\n%s", h.stdout)
		}
	})
	t.Run("encrypted", func(t *testing.T) {
		h := newHarness(t)
		for _, name := range []string{"supabase.json.enc", "cache-v6.json.enc"} {
			if err := os.WriteFile(filepath.Join(h.granolaDir, name), []byte{1, 2, 3}, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if code := h.run("doctor"); code != 1 {
			t.Fatalf("exit %d, want 1", code)
		}
		h.expectOutput("stores its login encrypted", "[encrypted by Granola]", "stores its cache encrypted")
	})
	t.Run("cache only", func(t *testing.T) {
		h := newHarness(t)
		h.writeCache()
		if code := h.run("doctor"); code != 0 {
			t.Fatalf("exit %d, want 0", code)
		}
		h.expectOutput("supabase.json is missing", "A sync would use the cache only")
	})
}

func TestVersionAndHelp(t *testing.T) {
	h := newHarness(t)
	if code := h.run("version"); code != 0 || !strings.Contains(h.stdout.String(), "granola-export "+version) {
		t.Errorf("version: exit %d, output %q", code, h.stdout)
	}
	if code := h.run("help"); code != 0 || !strings.Contains(h.stdout.String(), "schedule install") {
		t.Errorf("help: exit %d", code)
	}
	if code := h.run("sync", "--help"); code != 0 || !strings.Contains(h.stderr.String(), "--overwrite-edited") {
		t.Errorf("sync --help: exit %d, output %q", code, h.stderr)
	}
}
