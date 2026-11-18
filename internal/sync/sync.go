// Package sync mirrors Granola notes into a folder of Markdown files. It
// creates and updates files but never deletes one, and never overwrites a
// file that was edited after it was exported unless told to.
package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/YitianChenJKZ/granola-export/internal/granola"
	"github.com/YitianChenJKZ/granola-export/internal/obsidian"
)

const (
	// Granola may add the AI notes and transcript to a note without marking
	// it as updated, so young notes are always checked again.
	recentWindow = 7 * 24 * time.Hour

	maxConsecutiveFailures = 5
	frontmatterReadLimit   = 64 << 10

	// renderVersion is part of every source hash. Raising it makes the
	// next sync rewrite unedited files; do so when the layout changes.
	renderVersion = "1"
)

type Options struct {
	OutDir            string
	IncludeTranscript bool
	// Since, when set, limits the sync to notes created at or after it.
	Since           time.Time
	DryRun          bool
	OverwriteEdited bool
	Now             time.Time
	Location        *time.Location
	// Progress is called as the details of each note are fetched.
	Progress func(done, total int)
}

type Failure struct {
	ID     string
	Title  string
	Reason string
}

// Result reports what a run did. Paths are relative to the output folder.
type Result struct {
	Source string
	// Listed is how many notes were considered.
	Listed    int
	Created   []string
	Recreated []string
	Updated   []string
	// Edited are files left alone because they were changed after export
	// while the note also changed in Granola.
	Edited    []string
	Unchanged int
	// Empty counts notes with nothing to export.
	Empty int
	// Deferred counts notes changed in Granola that the cache cannot
	// export as fully as the app session did, so they wait for it.
	Deferred int
	Failed   []Failure
	Warnings []string
}

// ListError wraps a failure to list notes. Nothing has been written when it
// is returned, so the caller may try another source.
type ListError struct{ Err error }

func (e *ListError) Error() string { return e.Err.Error() }
func (e *ListError) Unwrap() error { return e.Err }

type job struct {
	note    *granola.Note
	entry   *entry
	rel     string
	hasFile bool
}

type runner struct {
	src      granola.Source
	o        Options
	st       *state
	index    map[string][]string
	res      *Result
	reserved map[string]bool
}

// Run syncs the notes from src into o.OutDir. The Result is returned even
// when the run stops early with an error.
func Run(ctx context.Context, src granola.Source, o Options) (*Result, error) {
	if o.Location == nil {
		o.Location = time.Local
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	res := &Result{Source: src.Name()}

	if info, err := os.Stat(o.OutDir); err == nil && !info.IsDir() {
		return res, fmt.Errorf("%s is not a folder", o.OutDir)
	}
	st, err := loadState(o.OutDir)
	if err != nil {
		return res, err
	}
	index, err := scanFolder(o.OutDir)
	if err != nil {
		return res, err
	}

	listed, err := src.List(ctx)
	if err != nil {
		return res, &ListError{err}
	}
	var notes []*granola.Note
	for _, n := range listed {
		if n.Deleted || (!o.Since.IsZero() && noteTime(n).Before(o.Since)) {
			continue
		}
		notes = append(notes, n)
	}
	// Oldest first, so that of two notes wanting the same file name the
	// earlier one gets it on every run.
	sort.SliceStable(notes, func(i, j int) bool {
		if !notes[i].CreatedAt.Equal(notes[j].CreatedAt) {
			return notes[i].CreatedAt.Before(notes[j].CreatedAt)
		}
		return notes[i].ID < notes[j].ID
	})
	res.Listed = len(notes)

	r := &runner{src: src, o: o, st: st, index: index, res: res, reserved: map[string]bool{}}
	if !o.DryRun {
		if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
			return res, err
		}
	}
	runErr := r.run(ctx, notes)
	if !o.DryRun {
		if runErr == nil {
			st.LastSync = &LastSync{
				Time: o.Now.UTC().Truncate(time.Second), Source: src.Name(),
				Created: len(res.Created) + len(res.Recreated), Updated: len(res.Updated),
				Unchanged: res.Unchanged, Skipped: len(res.Edited), Failed: len(res.Failed),
			}
		}
		if err := saveState(o.OutDir, st); err != nil && runErr == nil {
			runErr = fmt.Errorf("cannot save the sync record: %w", err)
		}
	}
	return res, runErr
}

func (r *runner) run(ctx context.Context, notes []*granola.Note) error {
	fromCache := r.src.Name() == granola.SourceCache
	var jobs []job
	toFetch := 0
	for _, n := range notes {
		e := r.st.Notes[n.ID]
		rel, hasFile := r.locate(n.ID, e)
		// The app session exports more than the cache holds (the AI
		// notes), so a cache run never replaces what it wrote.
		if fromCache && hasFile && e != nil && !e.Empty && e.Source == granola.SourceApp {
			if e.UpdatedAt != n.UpdatedRaw {
				r.res.Deferred++
			} else {
				r.res.Unchanged++
			}
			continue
		}
		if !n.Hydrated {
			if !r.needsDetails(n, e, hasFile) {
				if e.Empty {
					r.res.Empty++
				} else {
					r.res.Unchanged++
				}
				continue
			}
			toFetch++
		}
		jobs = append(jobs, job{note: n, entry: e, rel: rel, hasFile: hasFile})
	}

	fetched, failedInARow := 0, 0
	for _, j := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !j.note.Hydrated {
			err := r.src.Hydrate(ctx, j.note, r.o.IncludeTranscript)
			fetched++
			if r.o.Progress != nil {
				r.o.Progress(fetched, toFetch)
			}
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if errors.Is(err, granola.ErrAuth) {
					return err
				}
				r.fail(j.note, err.Error())
				if failedInARow++; failedInARow >= maxConsecutiveFailures {
					return fmt.Errorf("stopped after %d notes in a row could not be fetched; the last error was: %w", failedInARow, err)
				}
				continue
			}
			failedInARow = 0
		}
		r.apply(j)
	}
	return nil
}

// needsDetails decides whether a note's panels and transcript must be
// fetched, which costs requests, or whether the note can be taken as
// unchanged since the last sync.
func (r *runner) needsDetails(n *granola.Note, e *entry, hasFile bool) bool {
	switch {
	case e == nil:
		return true
	case e.UpdatedAt != n.UpdatedRaw, e.Source != r.src.Name():
		return true
	case !e.Empty && !hasFile:
		return true
	}
	return r.o.Now.Sub(noteTime(n)) < recentWindow
}

func noteTime(n *granola.Note) time.Time {
	if n.CreatedAt.IsZero() {
		return n.UpdatedAt
	}
	return n.CreatedAt
}

func (r *runner) fail(n *granola.Note, reason string) {
	r.res.Failed = append(r.res.Failed, Failure{ID: n.ID, Title: n.Title, Reason: reason})
}

// apply brings the file of one fully loaded note up to date.
func (r *runner) apply(j job) {
	n, e := j.note, j.entry
	withTranscript := r.o.IncludeTranscript

	// "Not found" for a part that was exported before more likely means
	// Granola's API changed than that the part is gone. Keep the file.
	if e != nil && !e.Empty {
		if n.PanelsUnavailable && e.HasSummary {
			r.fail(n, "Granola no longer returns the AI notes of this note; its file was left as it is")
			return
		}
		if withTranscript && n.TranscriptUnavailable && e.HasTranscript {
			r.fail(n, "Granola no longer returns the transcript of this note; its file was left as it is")
			return
		}
	}

	if n.Empty(withTranscript) {
		if j.hasFile {
			r.res.Unchanged++
			return
		}
		r.st.Notes[n.ID] = &entry{Empty: true, UpdatedAt: n.UpdatedRaw, Source: r.src.Name()}
		r.res.Empty++
		return
	}

	content := obsidian.Render(n, obsidian.Options{IncludeTranscript: withTranscript, Location: r.o.Location})
	// Hashed in a fixed time zone, so that travelling does not make every
	// note look changed.
	sourceHash := hash(renderVersion + "\n" + obsidian.Render(n, obsidian.Options{IncludeTranscript: withTranscript, Location: time.UTC}))
	next := &entry{
		Path:          j.rel,
		SourceHash:    sourceHash,
		FileHash:      hash(content),
		UpdatedAt:     n.UpdatedRaw,
		Source:        r.src.Name(),
		HasSummary:    len(n.Panels) > 0,
		HasTranscript: withTranscript && len(n.Transcript) > 0,
	}

	if !j.hasFile {
		next.Path = r.newPath(n)
		if err := r.write(next.Path, content, 0o644); err != nil {
			r.fail(n, err.Error())
			return
		}
		r.st.Notes[n.ID] = next
		if e != nil && !e.Empty {
			r.res.Recreated = append(r.res.Recreated, next.Path)
		} else {
			r.res.Created = append(r.res.Created, next.Path)
		}
		return
	}

	if e != nil && !e.Empty && e.SourceHash == sourceHash {
		e.UpdatedAt, e.Source, e.Path = n.UpdatedRaw, r.src.Name(), j.rel
		r.res.Unchanged++
		return
	}

	abs := filepath.Join(r.o.OutDir, filepath.FromSlash(j.rel))
	current, err := os.ReadFile(abs)
	if err != nil {
		r.fail(n, err.Error())
		return
	}
	untouched := e != nil && !e.Empty && e.FileHash == hash(string(current))
	switch {
	case string(current) == content:
		r.st.Notes[n.ID] = next
		r.res.Unchanged++
	case untouched || r.o.OverwriteEdited:
		mode := os.FileMode(0o644)
		if info, err := os.Stat(abs); err == nil {
			mode = info.Mode().Perm()
		}
		if err := r.write(j.rel, content, mode); err != nil {
			r.fail(n, err.Error())
			return
		}
		r.st.Notes[n.ID] = next
		r.res.Updated = append(r.res.Updated, j.rel)
	default:
		r.res.Edited = append(r.res.Edited, j.rel)
	}
}

// locate finds the file exported for a note by its granola_id property,
// wherever in the output folder it has been moved to.
func (r *runner) locate(id string, e *entry) (rel string, ok bool) {
	paths := r.index[id]
	if len(paths) == 0 {
		return "", false
	}
	rel = paths[0]
	if e != nil {
		for _, p := range paths {
			if p == e.Path {
				rel = p
			}
		}
	}
	if len(paths) > 1 {
		r.res.Warnings = append(r.res.Warnings, fmt.Sprintf("%d files carry granola_id %s; only %s is kept in sync", len(paths), id, rel))
	}
	return rel, true
}

// newPath picks a file name for a note that has no file yet, adding part of
// the note id when the plain name is taken.
func (r *runner) newPath(n *granola.Note) string {
	base := obsidian.BaseName(n, r.o.Location)
	candidates := []string{base}
	short := obsidian.ShortID(n.ID, 8)
	if short == "" {
		short = "note"
	}
	candidates = append(candidates, base+" ("+short+")")
	if long := obsidian.ShortID(n.ID, 64); long != short {
		candidates = append(candidates, base+" ("+long+")")
	}
	for i := 2; ; i++ {
		for _, c := range candidates {
			rel := c + ".md"
			if r.reserved[strings.ToLower(rel)] {
				continue
			}
			// Any error counts as free: if the name cannot even be
			// checked, the write reports why.
			if _, err := os.Lstat(filepath.Join(r.o.OutDir, rel)); err != nil {
				return rel
			}
		}
		candidates = []string{fmt.Sprintf("%s (%s) %d", base, short, i)}
	}
}

func (r *runner) write(rel, content string, mode os.FileMode) error {
	r.reserved[strings.ToLower(rel)] = true
	if r.o.DryRun {
		return nil
	}
	return writeFileAtomic(filepath.Join(r.o.OutDir, filepath.FromSlash(rel)), []byte(content), mode)
}

// scanFolder maps Granola note ids to the Markdown files below root that
// carry them. Hidden folders (.obsidian, .trash, the sync record) are
// skipped. A root that does not exist yet is simply empty.
func scanFolder(root string) (map[string][]string, error) {
	index := map[string][]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path != root {
				return nil
			}
			if errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(name, ".") || !strings.EqualFold(filepath.Ext(name), ".md") {
			return nil
		}
		head, err := readHead(path, frontmatterReadLimit)
		if err != nil {
			return nil
		}
		if id, ok := obsidian.ParseID(head); ok {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil
			}
			index[id] = append(index[id], filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, paths := range index {
		sort.Strings(paths)
	}
	return index, nil
}

func readHead(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}
