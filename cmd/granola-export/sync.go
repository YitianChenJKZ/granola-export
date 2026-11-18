package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"time"

	"github.com/YitianChenJKZ/granola-export/internal/granola"
	vaultsync "github.com/YitianChenJKZ/granola-export/internal/sync"
)

const timestampLayout = "2006-01-02 15:04"

func cmdSync(ctx context.Context, e *env, args []string) int {
	flags := e.newFlagSet("sync", "granola-export sync --out <folder> [flags]")
	out := flags.String("out", "", `folder to export into (default: the folder saved by "schedule install")`)
	source := flags.String("source", "auto", "where to read notes from: auto, app (Granola's login) or cache (Granola's local cache file)")
	noTranscript := flags.Bool("no-transcript", false, "leave transcripts out")
	since := flags.String("since", "", "only notes created on or after this date (YYYY-MM-DD)")
	dryRun := flags.Bool("dry-run", false, "show what would be written without writing anything")
	overwriteEdited := flags.Bool("overwrite-edited", false, "replace exported files you edited when their note changed in Granola")
	granolaDir := flags.String("granola-dir", e.granolaDir, "Granola's data folder")
	if code := e.parseFlags(flags, args); code >= 0 {
		return code
	}

	cfg, err := e.loadConfig()
	if err != nil {
		return e.fail("%v", err)
	}
	if *out == "" && cfg != nil {
		*out = cfg.Out
	}
	if cfg != nil && !flagWasSet(flags, "no-transcript") {
		*noTranscript = cfg.NoTranscript
	}
	if *out == "" {
		fmt.Fprintln(e.stderr, "granola-export: no output folder: pass --out <folder>")
		return 2
	}
	outDir, err := filepath.Abs(*out)
	if err != nil {
		return e.fail("%v", err)
	}
	if *source != "auto" && *source != granola.SourceApp && *source != granola.SourceCache {
		fmt.Fprintf(e.stderr, "granola-export: --source must be auto, app or cache, not %q\n", *source)
		return 2
	}
	opts := vaultsync.Options{
		OutDir:            outDir,
		IncludeTranscript: !*noTranscript,
		DryRun:            *dryRun,
		OverwriteEdited:   *overwriteEdited,
		Now:               e.now(),
		Location:          e.loc,
		Progress: func(done, total int) {
			if total >= 25 && (done%25 == 0 || done == total) {
				fmt.Fprintf(e.stderr, "Fetching note details: %d/%d\n", done, total)
			}
		},
	}
	if *since != "" {
		opts.Since, err = time.ParseInLocation("2006-01-02", *since, e.loc)
		if err != nil {
			fmt.Fprintf(e.stderr, "granola-export: --since must be a date like 2026-01-31, not %q\n", *since)
			return 2
		}
	}

	mode := ""
	if *dryRun {
		mode = " (dry run, nothing is written)"
	}
	fmt.Fprintf(e.stdout, "%s granola-export sync -> %s%s\n", e.now().In(e.loc).Format("2006-01-02 15:04:05"), outDir, mode)

	var src granola.Source
	var res *vaultsync.Result
	useCache := *source == granola.SourceCache

	if !useCache {
		app, appErr := e.appSource(*granolaDir)
		if appErr == nil {
			src = app
			res, err = vaultsync.Run(ctx, app, opts)
			var listErr *vaultsync.ListError
			if errors.As(err, &listErr) && ctx.Err() == nil {
				appErr = fmt.Errorf("cannot list notes with Granola's login: %w", listErr.Err)
			}
		}
		if appErr != nil {
			if *source == granola.SourceApp {
				return e.fail("%v", appErr)
			}
			fmt.Fprintf(e.stdout, "Granola's login cannot be used: %v\nFalling back to Granola's local cache file.\n", appErr)
			useCache = true
		}
	}
	if useCache {
		cache := e.cacheSource(*granolaDir)
		src = cache
		res, err = vaultsync.Run(ctx, cache, opts)
		var listErr *vaultsync.ListError
		if errors.As(err, &listErr) {
			return e.fail("%s", describeCacheError(listErr.Err, *granolaDir))
		}
	}

	e.report(res, src, opts)
	if err != nil {
		if errors.Is(err, granola.ErrAuth) {
			return e.fail("sync stopped: %v. Open Granola so it renews its login, then run the sync again.", err)
		}
		return e.fail("sync stopped: %v", err)
	}
	if len(res.Failed) > 0 {
		return 1
	}
	return 0
}

// appSource prepares reading notes with the login the Granola app keeps on
// disk. The token is used as it is and never refreshed: refreshing would
// replace the app's own session.
func (e *env) appSource(granolaDir string) (*granola.APISource, error) {
	token, err := granola.LoadToken(granolaDir)
	if err != nil {
		return nil, errors.New(granola.DescribeTokenError(err))
	}
	if token.Expired(e.now()) {
		return nil, fmt.Errorf("Granola's login expired at %s; open Granola so it renews it", token.ExpiresAt.In(e.loc).Format(timestampLayout))
	}
	client := granola.NewClient(e.apiBaseURL, token.AccessToken, "granola-export/"+version, e.appVersion)
	return granola.NewAPISource(client), nil
}

func (e *env) cacheSource(granolaDir string) *granola.CacheSource {
	src := &granola.CacheSource{Dir: granolaDir}
	if token, err := granola.LoadToken(granolaDir); err == nil {
		src.UserID = token.UserID
	}
	return src
}

func describeCacheError(err error, granolaDir string) string {
	switch {
	case errors.Is(err, granola.ErrEncrypted):
		return "Granola stores its cache encrypted on this Mac, which this tool does not unlock"
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Sprintf("no Granola cache file (cache-v*.json) found in %s; is Granola installed on this Mac?", granolaDir)
	default:
		return fmt.Sprintf("cannot read Granola's cache: %v", err)
	}
}

func (e *env) report(res *vaultsync.Result, src granola.Source, opts vaultsync.Options) {
	w := e.stdout
	verb := func(done, would string) string {
		if opts.DryRun {
			return would
		}
		return done
	}
	list := func(heading string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(w, "%s (%d):\n", heading, len(items))
		const limit = 20
		for i, item := range items {
			if i == limit {
				fmt.Fprintf(w, "  ... and %d more\n", len(items)-limit)
				break
			}
			fmt.Fprintf(w, "  %s\n", item)
		}
	}

	switch cache, isCache := src.(*granola.CacheSource); {
	case isCache:
		file := cache.File()
		fmt.Fprintf(w, "Source: Granola's local cache (%s, written %s), %d notes\n",
			filepath.Base(file.Path), file.ModTime.In(e.loc).Format(timestampLayout), res.Listed)
		fmt.Fprintln(w, "Notes exported from the cache can lack the AI-generated notes; they are completed by the next sync that can use Granola's login.")
	default:
		fmt.Fprintf(w, "Source: Granola's login, %d notes\n", res.Listed)
	}

	list(verb("Created", "Would create"), res.Created)
	list(verb("Created again, because the file was gone", "Would create again, because the file is gone"), res.Recreated)
	list(verb("Updated", "Would update"), res.Updated)
	list("Left alone: edited after export, and changed in Granola since (--overwrite-edited replaces them)", res.Edited)
	if len(res.Failed) > 0 {
		fmt.Fprintf(w, "Failed (%d):\n", len(res.Failed))
		for _, f := range res.Failed {
			title := f.Title
			if title == "" {
				title = "Untitled"
			}
			fmt.Fprintf(w, "  %s (%s): %s\n", title, f.ID, f.Reason)
		}
	}
	fmt.Fprintf(w, "Unchanged: %d\n", res.Unchanged)
	if res.Empty > 0 {
		fmt.Fprintf(w, "Skipped, nothing to export: %d\n", res.Empty)
	}
	if res.Deferred > 0 {
		fmt.Fprintf(w, "Changed in Granola but waiting for a sync that can use Granola's login: %d\n", res.Deferred)
	}
	for _, warning := range res.Warnings {
		fmt.Fprintf(w, "Warning: %s\n", warning)
	}
	for _, notice := range src.Notices() {
		fmt.Fprintf(w, "Note: %s\n", notice)
	}
}
