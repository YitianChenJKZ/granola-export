package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/YitianChenJKZ/granola-export/internal/granola"
)

var now = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

// fakeSource behaves like the app session when lazy (details cost a
// Hydrate call) and like the cache otherwise.
type fakeSource struct {
	name       string
	lazy       bool
	notes      []*granola.Note
	listErr    error
	hydrateErr map[string]error
	hydrated   []string
}

func (f *fakeSource) Name() string      { return f.name }
func (f *fakeSource) Notices() []string { return nil }

func (f *fakeSource) List(ctx context.Context) ([]*granola.Note, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []*granola.Note
	for _, n := range f.notes {
		c := *n
		c.Hydrated = !f.lazy
		if f.lazy {
			c.Panels, c.Transcript = nil, nil
			c.PanelsUnavailable, c.TranscriptUnavailable = false, false
		}
		out = append(out, &c)
	}
	return out, nil
}

func (f *fakeSource) Hydrate(ctx context.Context, n *granola.Note, withTranscript bool) error {
	f.hydrated = append(f.hydrated, n.ID)
	if err := f.hydrateErr[n.ID]; err != nil {
		return err
	}
	for _, full := range f.notes {
		if full.ID == n.ID {
			n.Panels = full.Panels
			n.PanelsUnavailable = full.PanelsUnavailable
			if withTranscript {
				n.Transcript = full.Transcript
				n.TranscriptUnavailable = full.TranscriptUnavailable
			}
		}
	}
	n.Hydrated = true
	return nil
}

func (f *fakeSource) note(id string) *granola.Note {
	for _, n := range f.notes {
		if n.ID == id {
			return n
		}
	}
	panic("no note " + id)
}

// touch marks a note as changed in Granola.
func (f *fakeSource) touch(id string) *granola.Note {
	n := f.note(id)
	n.UpdatedAt = n.UpdatedAt.Add(time.Minute)
	n.UpdatedRaw = n.UpdatedAt.Format(time.RFC3339)
	return n
}

func note(id, title string, age time.Duration, summary string) *granola.Note {
	created := now.Add(-age)
	n := &granola.Note{
		ID: id, Title: title, CreatedAt: created, UpdatedAt: created.Add(time.Hour),
		UpdatedRaw: created.Add(time.Hour).Format(time.RFC3339),
	}
	if summary != "" {
		n.Panels = []granola.Panel{{Title: "Summary", Markdown: summary}}
		n.Transcript = []granola.Segment{{Start: created, Source: "microphone", Text: "words about " + title}}
	}
	return n
}

const day = 24 * time.Hour

func appSource() *fakeSource {
	return &fakeSource{name: granola.SourceApp, lazy: true, notes: []*granola.Note{
		note("old", "Old planning", 30*day, "old summary"),
		note("new", "Fresh standup", 1*day, "fresh summary"),
	}}
}

func opts(dir string) Options {
	return Options{OutDir: dir, IncludeTranscript: true, Now: now, Location: time.UTC}
}

func mustRun(t *testing.T, src granola.Source, o Options) *Result {
	t.Helper()
	res, err := Run(context.Background(), src, o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Failed) > 0 {
		t.Fatalf("Run reported failures: %+v", res.Failed)
	}
	return res
}

// tree returns every file below dir (except the sync record) with its content.
func tree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == StateDir {
				return filepath.SkipDir
			}
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func names(files map[string]string) []string {
	var out []string
	for name := range files {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const (
	oldFile = "2026-10-19 Old planning.md"
	newFile = "2026-11-17 Fresh standup.md"
)

func TestFirstRunCreatesFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "vault", "Granola")
	src := appSource()
	src.notes = append(src.notes,
		note("empty", "Calendar placeholder", 2*day, ""),
		&granola.Note{ID: "gone", Title: "Deleted in Granola", Deleted: true, CreatedAt: now.Add(-day), NotesMarkdown: "x"},
	)
	res := mustRun(t, src, opts(dir))

	if !reflect.DeepEqual(res.Created, []string{oldFile, newFile}) || res.Empty != 1 || res.Listed != 3 {
		t.Errorf("result = %+v", res)
	}
	files := tree(t, dir)
	if !reflect.DeepEqual(names(files), []string{oldFile, newFile}) {
		t.Fatalf("files = %q", names(files))
	}
	for _, want := range []string{"granola_id: \"new\"", "## Summary\n\nfresh summary", "> [!quote]- Transcript", "words about Fresh standup"} {
		if !strings.Contains(files[newFile], want) {
			t.Errorf("%s lacks %q:\n%s", newFile, want, files[newFile])
		}
	}
	if _, err := os.Stat(filepath.Join(dir, StateDir, "state.json")); err != nil {
		t.Errorf("no sync record written: %v", err)
	}
	last, err := ReadLastSync(dir)
	if err != nil || last == nil || last.Created != 2 || last.Source != granola.SourceApp || !last.Time.Equal(now) {
		t.Errorf("last sync = %+v, %v", last, err)
	}
}

func TestSecondRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	src := appSource()
	src.notes = append(src.notes, note("empty-old", "Old placeholder", 40*day, ""))
	mustRun(t, src, opts(dir))
	before := tree(t, dir)
	past := now.Add(-time.Hour)
	for name := range before {
		if err := os.Chtimes(filepath.Join(dir, name), past, past); err != nil {
			t.Fatal(err)
		}
	}

	src.hydrated = nil
	res := mustRun(t, src, opts(dir))
	if len(res.Created)+len(res.Recreated)+len(res.Updated)+len(res.Edited) != 0 || res.Unchanged != 2 || res.Empty != 1 {
		t.Errorf("second run result = %+v", res)
	}
	if !reflect.DeepEqual(tree(t, dir), before) {
		t.Error("file contents changed on the second run")
	}
	for name := range before {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.ModTime().Equal(past) {
			t.Errorf("%s was rewritten on the second run", name)
		}
	}
	// Only the note from the last week is fetched again.
	if !reflect.DeepEqual(src.hydrated, []string{"new"}) {
		t.Errorf("details fetched for %q, want only the recent note", src.hydrated)
	}
	for _, leftover := range names(tree(t, dir)) {
		if strings.Contains(leftover, ".tmp") {
			t.Errorf("temporary file left behind: %s", leftover)
		}
	}
}

func TestChangedNoteUpdatesItsFileInPlace(t *testing.T) {
	dir := t.TempDir()
	src := appSource()
	mustRun(t, src, opts(dir))

	n := src.touch("old")
	n.Title = "Old planning (renamed in Granola)"
	n.Panels[0].Markdown = "revised summary"
	src.hydrated = nil
	res := mustRun(t, src, opts(dir))

	if !reflect.DeepEqual(res.Updated, []string{oldFile}) || len(res.Created) != 0 {
		t.Errorf("result = %+v", res)
	}
	files := tree(t, dir)
	if !reflect.DeepEqual(names(files), []string{oldFile, newFile}) {
		t.Errorf("files = %q; the file name must not follow the title", names(files))
	}
	if !strings.Contains(files[oldFile], `title: "Old planning (renamed in Granola)"`) || !strings.Contains(files[oldFile], "revised summary") {
		t.Errorf("file not updated:\n%s", files[oldFile])
	}
	if !reflect.DeepEqual(src.hydrated, []string{"old", "new"}) {
		t.Errorf("details fetched for %q", src.hydrated)
	}
}

func TestEditedFileIsNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	src := appSource()
	mustRun(t, src, opts(dir))
	path := filepath.Join(dir, oldFile)
	edited := read(t, path) + "\nMy own follow-up thoughts.\n"
	write(t, path, edited)

	// Edited, but unchanged in Granola: left alone without comment.
	res := mustRun(t, src, opts(dir))
	if len(res.Edited) != 0 || len(res.Updated) != 0 || read(t, path) != edited {
		t.Errorf("an edited file of an unchanged note was touched or reported: %+v", res)
	}

	// Edited and changed in Granola: skipped and reported, on every run.
	src.touch("old").Panels[0].Markdown = "revised summary"
	for i := 0; i < 2; i++ {
		res = mustRun(t, src, opts(dir))
		if !reflect.DeepEqual(res.Edited, []string{oldFile}) || len(res.Updated) != 0 {
			t.Errorf("run %d result = %+v", i, res)
		}
		if read(t, path) != edited {
			t.Fatalf("run %d overwrote the edited file", i)
		}
	}

	o := opts(dir)
	o.OverwriteEdited = true
	res = mustRun(t, src, o)
	if !reflect.DeepEqual(res.Updated, []string{oldFile}) || len(res.Edited) != 0 {
		t.Errorf("forced run result = %+v", res)
	}
	if got := read(t, path); strings.Contains(got, "follow-up thoughts") || !strings.Contains(got, "revised summary") {
		t.Errorf("forced run did not replace the file:\n%s", got)
	}

	res = mustRun(t, src, opts(dir))
	if len(res.Edited)+len(res.Updated) != 0 {
		t.Errorf("run after forced overwrite = %+v", res)
	}
}

func TestMovedAndRenamedFileIsUpdatedWhereItIs(t *testing.T) {
	dir := t.TempDir()
	src := appSource()
	mustRun(t, src, opts(dir))
	moved := filepath.Join(dir, "Meetings", "2026", "Planning (old).md")
	if err := os.MkdirAll(filepath.Dir(moved), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, oldFile), moved); err != nil {
		t.Fatal(err)
	}

	res := mustRun(t, src, opts(dir))
	if len(res.Created)+len(res.Recreated)+len(res.Updated) != 0 {
		t.Errorf("moving a file caused writes: %+v", res)
	}

	src.touch("old").Panels[0].Markdown = "revised summary"
	res = mustRun(t, src, opts(dir))
	if !reflect.DeepEqual(res.Updated, []string{"Meetings/2026/Planning (old).md"}) {
		t.Errorf("result = %+v", res)
	}
	files := tree(t, dir)
	if !reflect.DeepEqual(names(files), []string{newFile, "Meetings/2026/Planning (old).md"}) {
		t.Errorf("files = %q", names(files))
	}
	if !strings.Contains(read(t, moved), "revised summary") {
		t.Error("moved file was not updated")
	}
}

func TestDeletedFileIsRecreatedAndDeletedNoteKeepsItsFile(t *testing.T) {
	dir := t.TempDir()
	src := appSource()
	mustRun(t, src, opts(dir))

	if err := os.Remove(filepath.Join(dir, oldFile)); err != nil {
		t.Fatal(err)
	}
	src.note("new").Deleted = true
	res := mustRun(t, src, opts(dir))
	if !reflect.DeepEqual(res.Recreated, []string{oldFile}) || len(res.Created) != 0 {
		t.Errorf("result = %+v", res)
	}
	if !reflect.DeepEqual(names(tree(t, dir)), []string{oldFile, newFile}) {
		t.Errorf("files = %q; a note deleted in Granola must keep its file", names(tree(t, dir)))
	}

	src.notes = src.notes[:1]
	mustRun(t, src, opts(dir))
	if _, err := os.Stat(filepath.Join(dir, newFile)); err != nil {
		t.Errorf("file of a note that vanished from Granola was removed: %v", err)
	}
}

func TestFileNameCollisions(t *testing.T) {
	dir := t.TempDir()
	unrelated := filepath.Join(dir, "2026-11-17 Weekly sync.md")
	write(t, unrelated, "my own note, not from Granola\n")

	a := note("aaaaaaaa-1111", "Weekly sync", 1*day, "first")
	b := note("bbbbbbbb-2222", "Weekly sync", 1*day, "second")
	b.CreatedAt = b.CreatedAt.Add(time.Hour)
	c := note("bbbbbbbb-3333", "Weekly: sync?", 1*day, "third")
	c.CreatedAt = c.CreatedAt.Add(2 * time.Hour)
	src := &fakeSource{name: granola.SourceApp, lazy: true, notes: []*granola.Note{c, b, a}}

	res := mustRun(t, src, opts(dir))
	want := []string{
		"2026-11-17 Weekly sync (aaaaaaaa).md",
		"2026-11-17 Weekly sync (bbbbbbbb).md",
		"2026-11-17 Weekly sync (bbbbbbbb3333).md",
	}
	if !reflect.DeepEqual(res.Created, want) {
		t.Errorf("created %q, want %q", res.Created, want)
	}
	if read(t, unrelated) != "my own note, not from Granola\n" {
		t.Error("an unrelated file with the same name was overwritten")
	}

	res = mustRun(t, src, opts(dir))
	if len(res.Created)+len(res.Updated) != 0 {
		t.Errorf("second run wrote again: %+v", res)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "not-created")
	o := opts(dir)
	o.DryRun = true
	src := appSource()
	src.notes = append(src.notes, note("twin", "Old planning", 30*day, "twin summary"))
	res := mustRun(t, src, o)
	if len(res.Created) != 3 || res.Created[1] == res.Created[2] {
		t.Errorf("dry run should report three distinct files: %+v", res)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("dry run created the output folder (%v)", err)
	}

	real := t.TempDir()
	mustRun(t, appSource(), opts(real))
	before := tree(t, real)
	record := read(t, filepath.Join(real, StateDir, "state.json"))
	changed := appSource()
	changed.touch("old").Panels[0].Markdown = "revised"
	o = opts(real)
	o.DryRun = true
	res = mustRun(t, changed, o)
	if !reflect.DeepEqual(res.Updated, []string{oldFile}) {
		t.Errorf("dry run result = %+v", res)
	}
	if !reflect.DeepEqual(tree(t, real), before) || read(t, filepath.Join(real, StateDir, "state.json")) != record {
		t.Error("dry run changed files or the sync record")
	}
}

func cacheSource(from *fakeSource) *fakeSource {
	c := &fakeSource{name: granola.SourceCache}
	for _, n := range from.notes {
		copied := *n
		copied.Panels = nil // caches since v4 hold no AI notes
		copied.NotesMarkdown = "typed notes for " + n.Title
		c.notes = append(c.notes, &copied)
	}
	return c
}

func TestCacheNeverDowngradesAnAppExport(t *testing.T) {
	dir := t.TempDir()
	app := appSource()
	app.note("old").NotesMarkdown = "typed notes for Old planning"
	app.note("new").NotesMarkdown = "typed notes for Fresh standup"
	mustRun(t, app, opts(dir))
	before := tree(t, dir)

	cache := cacheSource(app)
	cache.touch("new")
	cache.notes = append(cache.notes, note("offline", "Made while offline", 0, ""))
	cache.note("offline").NotesMarkdown = "typed offline"
	res := mustRun(t, cache, opts(dir))
	if res.Deferred != 1 || res.Unchanged != 1 || len(res.Updated) != 0 || len(res.Created) != 1 {
		t.Errorf("cache run result = %+v", res)
	}
	after := tree(t, dir)
	for name, content := range before {
		if after[name] != content {
			t.Errorf("%s was changed by a cache run", name)
		}
	}

	// Back online: the note first exported from the cache gains its summary.
	app.touch("new")
	offline := note("offline", "Made while offline", 0, "summary that the cache lacked")
	offline.NotesMarkdown = "typed offline"
	app.notes = append(app.notes, offline)
	res = mustRun(t, app, opts(dir))
	if len(res.Updated) != 2 || len(res.Edited) != 0 {
		t.Errorf("app run after cache run = %+v", res)
	}
	if got := read(t, filepath.Join(dir, "2026-11-18 Made while offline.md")); !strings.Contains(got, "summary that the cache lacked") {
		t.Errorf("cache-written file was not completed by the app run:\n%s", got)
	}
}

func TestEmptyNotesAreRememberedUntilTheyChange(t *testing.T) {
	dir := t.TempDir()
	src := &fakeSource{name: granola.SourceApp, lazy: true, notes: []*granola.Note{note("ph", "Placeholder", 30*day, "")}}
	res := mustRun(t, src, opts(dir))
	if res.Empty != 1 || len(tree(t, dir)) != 0 {
		t.Fatalf("result = %+v, files = %q", res, names(tree(t, dir)))
	}

	src.hydrated = nil
	res = mustRun(t, src, opts(dir))
	if res.Empty != 1 || len(src.hydrated) != 0 {
		t.Errorf("old empty note was fetched again (%q): %+v", src.hydrated, res)
	}

	src.touch("ph").Panels = []granola.Panel{{Title: "Summary", Markdown: "now has content"}}
	res = mustRun(t, src, opts(dir))
	if !reflect.DeepEqual(res.Created, []string{"2026-10-19 Placeholder.md"}) {
		t.Errorf("result = %+v", res)
	}
}

func TestTranscriptOnlyNoteIsEmptyWithoutTranscripts(t *testing.T) {
	dir := t.TempDir()
	n := note("t", "Just talk", 1*day, "")
	n.Transcript = []granola.Segment{{Start: n.CreatedAt, Source: "system", Text: "hello"}}
	src := &fakeSource{name: granola.SourceApp, lazy: true, notes: []*granola.Note{n}}

	o := opts(dir)
	o.IncludeTranscript = false
	if res := mustRun(t, src, o); res.Empty != 1 || len(res.Created) != 0 {
		t.Errorf("without transcripts: %+v", res)
	}
	if res := mustRun(t, src, opts(dir)); len(res.Created) != 1 {
		t.Errorf("with transcripts: %+v", res)
	}
}

func TestFailedNotesDoNotStopTheRun(t *testing.T) {
	dir := t.TempDir()
	src := appSource()
	src.hydrateErr = map[string]error{"old": errors.New("HTTP 500")}
	res, err := Run(context.Background(), src, opts(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Failed) != 1 || res.Failed[0].ID != "old" || !reflect.DeepEqual(res.Created, []string{newFile}) {
		t.Errorf("result = %+v", res)
	}

	// The failed note is retried and exported once the error clears.
	src.hydrateErr = nil
	res = mustRun(t, src, opts(dir))
	if !reflect.DeepEqual(res.Created, []string{oldFile}) {
		t.Errorf("retry result = %+v", res)
	}
}

func TestRunStopsOnRepeatedOrAuthFailures(t *testing.T) {
	many := &fakeSource{name: granola.SourceApp, lazy: true, hydrateErr: map[string]error{}}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("n%02d", i)
		many.notes = append(many.notes, note(id, "Note "+id, time.Duration(i+1)*day, "s"))
		many.hydrateErr[id] = errors.New("HTTP 500")
	}
	res, err := Run(context.Background(), many, opts(t.TempDir()))
	if err == nil || len(many.hydrated) != maxConsecutiveFailures || len(res.Failed) != maxConsecutiveFailures {
		t.Errorf("error = %v after %d attempts", err, len(many.hydrated))
	}

	dir := t.TempDir()
	auth := appSource()
	auth.hydrateErr = map[string]error{"new": fmt.Errorf("panels: %w", granola.ErrAuth)}
	res, err = Run(context.Background(), auth, opts(dir))
	if !errors.Is(err, granola.ErrAuth) || !reflect.DeepEqual(res.Created, []string{oldFile}) {
		t.Errorf("error = %v, result = %+v", err, res)
	}
	// What was written before the stop is on record, so it is not redone.
	if last, _ := ReadLastSync(dir); last != nil {
		t.Errorf("an aborted run was recorded as the last sync: %+v", last)
	}
	auth.hydrateErr = nil
	res = mustRun(t, auth, opts(dir))
	if !reflect.DeepEqual(res.Created, []string{newFile}) || res.Unchanged != 1 {
		t.Errorf("run after the aborted one = %+v", res)
	}
}

func TestListFailureWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "out")
	src := &fakeSource{name: granola.SourceApp, listErr: errors.New("offline")}
	_, err := Run(context.Background(), src, opts(dir))
	var listErr *ListError
	if !errors.As(err, &listErr) {
		t.Fatalf("error = %v, want a ListError", err)
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Error("output folder was created although listing failed")
	}
}

func TestSince(t *testing.T) {
	dir := t.TempDir()
	o := opts(dir)
	o.Since = now.Add(-7 * day)
	res := mustRun(t, appSource(), o)
	if !reflect.DeepEqual(res.Created, []string{newFile}) || res.Listed != 1 {
		t.Errorf("result = %+v", res)
	}
}

func TestMissingPartsDoNotStripAnExportedFile(t *testing.T) {
	dir := t.TempDir()
	src := appSource()
	src.note("new").NotesMarkdown = "typed"
	mustRun(t, src, opts(dir))
	before := read(t, filepath.Join(dir, newFile))

	n := src.touch("new")
	saved := n.Panels
	n.Panels, n.PanelsUnavailable = nil, true
	res, err := Run(context.Background(), src, opts(dir))
	if err != nil || len(res.Failed) != 1 || len(res.Updated) != 0 {
		t.Errorf("result = %+v, %v", res, err)
	}
	if read(t, filepath.Join(dir, newFile)) != before {
		t.Error("file lost its summary after Granola answered not-found")
	}

	n.Panels, n.PanelsUnavailable = saved, false
	n.Transcript, n.TranscriptUnavailable = nil, true
	res, err = Run(context.Background(), src, opts(dir))
	if err != nil || len(res.Failed) != 1 || read(t, filepath.Join(dir, newFile)) != before {
		t.Errorf("transcript not-found: result = %+v, %v", res, err)
	}

	// A note that never had the part is exported without it.
	fresh := note("fresh", "No summary yet", 0, "")
	fresh.NotesMarkdown, fresh.PanelsUnavailable = "typed", true
	src.notes = []*granola.Note{fresh}
	res = mustRun(t, src, opts(dir))
	if len(res.Created) != 1 {
		t.Errorf("result = %+v", res)
	}
}

func TestLostSyncRecord(t *testing.T) {
	dir := t.TempDir()
	src := appSource()
	mustRun(t, src, opts(dir))
	if err := os.RemoveAll(filepath.Join(dir, StateDir)); err != nil {
		t.Fatal(err)
	}
	edited := read(t, filepath.Join(dir, newFile)) + "\nedited\n"
	write(t, filepath.Join(dir, newFile), edited)

	res := mustRun(t, src, opts(dir))
	if len(res.Created)+len(res.Updated) != 0 || res.Unchanged != 1 || !reflect.DeepEqual(res.Edited, []string{newFile}) {
		t.Errorf("result = %+v", res)
	}
	if read(t, filepath.Join(dir, newFile)) != edited {
		t.Error("a file that differs from the export was overwritten without a sync record")
	}

	// The identical file was adopted, so a later change updates it.
	src.touch("old").Panels[0].Markdown = "revised"
	res = mustRun(t, src, opts(dir))
	if !reflect.DeepEqual(res.Updated, []string{oldFile}) {
		t.Errorf("result after adoption = %+v", res)
	}
}

func TestTimeZoneChangeDoesNotRewriteFiles(t *testing.T) {
	dir := t.TempDir()
	src := appSource()
	mustRun(t, src, opts(dir))
	before := tree(t, dir)

	o := opts(dir)
	o.Location = time.FixedZone("JST", 9*3600)
	res := mustRun(t, src, o)
	if len(res.Updated)+len(res.Edited)+len(res.Created) != 0 || !reflect.DeepEqual(tree(t, dir), before) {
		t.Errorf("result in another time zone = %+v", res)
	}
}

func TestDuplicateIDsAndHiddenFolders(t *testing.T) {
	dir := t.TempDir()
	src := appSource()
	mustRun(t, src, opts(dir))
	original := read(t, filepath.Join(dir, oldFile))
	write(t, filepath.Join(dir, "Copies", "copy.md"), original)
	write(t, filepath.Join(dir, ".trash", "trashed.md"), original)
	write(t, filepath.Join(dir, ".obsidian", "workspace.md"), original)

	src.touch("old").Panels[0].Markdown = "revised"
	res := mustRun(t, src, opts(dir))
	if !reflect.DeepEqual(res.Updated, []string{oldFile}) {
		t.Errorf("result = %+v", res)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "2 files") {
		t.Errorf("warnings = %q, want one about the visible duplicate only", res.Warnings)
	}
	for _, untouched := range []string{"Copies/copy.md", ".trash/trashed.md", ".obsidian/workspace.md"} {
		if read(t, filepath.Join(dir, filepath.FromSlash(untouched))) != original {
			t.Errorf("%s was modified", untouched)
		}
	}
}

func TestOnlyFilesWithTheIDPropertyAreTouched(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "notes.md"), "# Mine\n\ngranola_id: old\n")
	write(t, filepath.Join(dir, "data.txt"), "---\ngranola_id: old\n---\n")
	mustRun(t, appSource(), opts(dir))
	if read(t, filepath.Join(dir, "notes.md")) != "# Mine\n\ngranola_id: old\n" || read(t, filepath.Join(dir, "data.txt")) != "---\ngranola_id: old\n---\n" {
		t.Error("a file without the frontmatter property, or not Markdown, was modified")
	}
	if _, err := os.Stat(filepath.Join(dir, oldFile)); err != nil {
		t.Errorf("note was not exported: %v", err)
	}
}

func TestDamagedSyncRecordStopsTheRun(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, StateDir, "state.json"), "{ not json")
	if _, err := Run(context.Background(), appSource(), opts(dir)); err == nil || !strings.Contains(err.Error(), "damaged") {
		t.Errorf("error = %v", err)
	}
	write(t, filepath.Join(dir, StateDir, "state.json"), `{"version": 99, "notes": {}}`)
	if _, err := Run(context.Background(), appSource(), opts(dir)); err == nil || !strings.Contains(err.Error(), "newer version") {
		t.Errorf("error = %v", err)
	}
	if len(tree(t, dir)) != 0 {
		t.Error("files were written despite an unusable sync record")
	}
}

func TestCancelledRunStopsAndKeepsProgress(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	src := appSource()
	o := opts(dir)
	o.Progress = func(done, total int) {
		if total != 2 {
			t.Errorf("progress total = %d, want 2", total)
		}
		cancel()
	}
	res, err := Run(ctx, src, o)
	if !errors.Is(err, context.Canceled) || len(res.Created) != 1 {
		t.Errorf("error = %v, result = %+v", err, res)
	}
	res = mustRun(t, src, opts(dir))
	if len(res.Created) != 1 || res.Unchanged != 1 {
		t.Errorf("resumed run = %+v", res)
	}
}

func TestOutputPathThatIsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "file.md")
	write(t, path, "x")
	if _, err := Run(context.Background(), appSource(), opts(path)); err == nil {
		t.Error("expected an error when the output path is a file")
	}
}

func TestOverwritePreservesFileMode(t *testing.T) {
	dir := t.TempDir()
	src := appSource()
	mustRun(t, src, opts(dir))
	path := filepath.Join(dir, oldFile)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	src.touch("old").Panels[0].Markdown = "revised"
	mustRun(t, src, opts(dir))
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode after update = %v, %v", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(filepath.Join(dir, newFile)); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("mode of a new file = %v, %v", info.Mode().Perm(), err)
	}
}
