package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/YitianChenJKZ/granola-export/internal/granola"
)

// cmdDoctor reports what the tool can read on this Mac. It prints counts,
// versions, key names and times only: never the login token or note content.
func cmdDoctor(e *env, args []string) int {
	flags := e.newFlagSet("doctor", "granola-export doctor [flags]")
	granolaDir := flags.String("granola-dir", e.granolaDir, "Granola's data folder")
	if code := e.parseFlags(flags, args); code >= 0 {
		return code
	}
	w := e.stdout
	dir := *granolaDir

	fmt.Fprintf(w, "Granola data folder: %s\n", dir)
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		fmt.Fprintln(w, "  NOT FOUND. Is Granola installed and has it been opened on this Mac?")
		return 1
	}
	if e.appVersion != "" {
		fmt.Fprintf(w, "Granola app: version %s\n", e.appVersion)
	} else {
		fmt.Fprintln(w, "Granola app: not found in /Applications or ~/Applications (requests are sent without its version number)")
	}

	fmt.Fprintln(w, "\nLogin (supabase.json):")
	appUsable := false
	token, err := granola.LoadToken(dir)
	switch {
	case err != nil:
		fmt.Fprintf(w, "  NOT USABLE: %s\n", granola.DescribeTokenError(err))
	case token.ExpiresAt.IsZero():
		appUsable = true
		fmt.Fprintln(w, "  present; its expiry time could not be read, so a sync will simply try it")
	case token.Expired(e.now()):
		fmt.Fprintf(w, "  present, but EXPIRED at %s. Open Granola so it renews it.\n", token.ExpiresAt.In(e.loc).Format(timestampLayout))
	default:
		appUsable = true
		fmt.Fprintf(w, "  present; valid until %s (%s from now)\n",
			token.ExpiresAt.In(e.loc).Format(timestampLayout), token.ExpiresAt.Sub(e.now()).Round(time.Minute))
	}

	fmt.Fprintln(w, "\nLocal cache:")
	cacheUsable := doctorCache(e, dir)

	fmt.Fprintln(w, "\nResult:")
	switch {
	case appUsable && cacheUsable:
		fmt.Fprintln(w, "  A sync would use Granola's login, with the cache as fallback.")
	case appUsable:
		fmt.Fprintln(w, "  A sync would use Granola's login. There is no usable cache to fall back on.")
	case cacheUsable:
		fmt.Fprintln(w, "  A sync would use the cache only: typed notes and transcripts, and AI notes only where the cache holds them.")
	default:
		fmt.Fprintln(w, "  Nothing a sync could read from.")
		return 1
	}
	fmt.Fprintln(w, `  "granola-export sync --out <folder> --dry-run" shows what a sync would write, without writing anything.`)
	return 0
}

func doctorCache(e *env, dir string) (usable bool) {
	w := e.stdout
	caches, err := granola.FindCaches(dir)
	if err != nil {
		fmt.Fprintf(w, "  NOT USABLE: %v\n", err)
		return false
	}
	if len(caches) == 0 {
		fmt.Fprintln(w, "  NOT FOUND: no cache-v*.json in the data folder")
		return false
	}
	for i, c := range caches {
		note := ""
		switch {
		case c.Encrypted:
			note = "  [encrypted by Granola]"
		case i > 0:
			note = "  [older format, ignored]"
		}
		fmt.Fprintf(w, "  %s: %.1f MB, written %s%s\n", filepath.Base(c.Path), float64(c.Size)/(1<<20), c.ModTime.In(e.loc).Format(timestampLayout), note)
	}
	newest := caches[0]
	if newest.Encrypted {
		fmt.Fprintln(w, "  NOT USABLE: Granola stores its cache encrypted on this Mac, which this tool does not unlock")
		return false
	}
	cache, err := granola.LoadCache(newest.Path)
	if err != nil {
		var shape *granola.ShapeError
		switch {
		case errors.As(err, &shape):
			fmt.Fprintf(w, "  NOT USABLE: layout not recognised: %s\n", shape.Detail)
		case errors.Is(err, fs.ErrPermission):
			fmt.Fprintln(w, "  NOT USABLE: no permission to read the file")
		default:
			fmt.Fprintf(w, "  NOT USABLE: %v\n", err)
		}
		return false
	}
	layout := "stored directly"
	if cache.Encoding == "string" {
		layout = "stored as a JSON-encoded string"
	}
	stats := cache.Stats()
	fmt.Fprintf(w, "  layout recognised: format version %d, %s\n", cache.Version, layout)
	fmt.Fprintf(w, "  documents:           %d\n", stats.Documents)
	fmt.Fprintf(w, "    deleted:           %d\n", stats.Deleted)
	fmt.Fprintf(w, "    with typed notes:  %d\n", stats.WithTypedNotes)
	fmt.Fprintf(w, "    with AI notes:     %d\n", stats.WithPanels)
	fmt.Fprintf(w, "    with a transcript: %d\n", stats.WithTranscript)
	if stats.Unreadable > 0 {
		fmt.Fprintf(w, "    NOT READABLE:      %d (their layout is not recognised)\n", stats.Unreadable)
	}
	return stats.Documents == 0 || stats.Unreadable < stats.Documents
}
