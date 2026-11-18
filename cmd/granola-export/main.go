// Command granola-export mirrors notes from the Granola Mac app into a folder
// of Obsidian-compatible Markdown files and can schedule itself to run daily.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/YitianChenJKZ/granola-export/internal/granola"
	"github.com/YitianChenJKZ/granola-export/internal/schedule"
)

var version = "dev"

const usage = `granola-export exports Granola notes as Obsidian-compatible Markdown.

Usage:
  granola-export sync --out <folder> [flags]      Export notes now
  granola-export schedule install --out <folder>  Sync every day (launchd)
  granola-export schedule uninstall               Remove the daily job
  granola-export schedule status                  Show the daily job
  granola-export doctor                           Check what the tool can read
  granola-export version

Run "granola-export <command> --help" for the flags of a command.
`

// env carries everything the commands take from the outside world, so tests
// can run them against temporary folders and a local stand-in for the API.
type env struct {
	stdout, stderr io.Writer
	home           string
	granolaDir     string
	configDir      string
	apiBaseURL     string
	appVersion     string
	goos           string
	uid            int
	loc            *time.Location
	now            func() time.Time
	executable     func() (string, error)
	isTemporary    func(path string) bool
	sleep          func(time.Duration)
	runCommand     func(name string, args ...string) ([]byte, error)
}

func defaultEnv() (*env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("cannot find your home folder: %w", err)
	}
	configRoot, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("cannot find your settings folder: %w", err)
	}
	return &env{
		stdout:      os.Stdout,
		stderr:      os.Stderr,
		home:        home,
		granolaDir:  granola.DefaultDir(home),
		configDir:   filepath.Join(configRoot, "granola-export"),
		apiBaseURL:  granola.DefaultBaseURL,
		appVersion:  granola.DetectAppVersion(home),
		goos:        runtime.GOOS,
		uid:         os.Getuid(),
		loc:         time.Local,
		now:         time.Now,
		executable:  os.Executable,
		isTemporary: schedule.IsTemporaryPath,
		sleep:       time.Sleep,
		runCommand: func(name string, args ...string) ([]byte, error) {
			return exec.Command(name, args...).CombinedOutput()
		},
	}, nil
}

func main() {
	e, err := defaultEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "granola-export:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, e, os.Args[1:]))
}

// run returns the process exit code: 0 success, 1 failure, 2 usage error.
func run(ctx context.Context, e *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usage)
		return 2
	}
	switch args[0] {
	case "sync":
		return cmdSync(ctx, e, args[1:])
	case "schedule":
		return cmdSchedule(e, args[1:])
	case "doctor":
		return cmdDoctor(e, args[1:])
	case "version", "--version", "-version":
		fmt.Fprintln(e.stdout, "granola-export", version)
		return 0
	case "help", "--help", "-help", "-h":
		fmt.Fprint(e.stdout, usage)
		return 0
	default:
		fmt.Fprintf(e.stderr, "granola-export: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
