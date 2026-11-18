package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/YitianChenJKZ/granola-export/internal/schedule"
	vaultsync "github.com/YitianChenJKZ/granola-export/internal/sync"
)

const scheduleUsage = `Usage:
  granola-export schedule install --out <folder> [--at HH:MM] [--no-transcript]
  granola-export schedule uninstall
  granola-export schedule status
`

func cmdSchedule(e *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, scheduleUsage)
		return 2
	}
	manager := &schedule.Manager{Home: e.home, UID: e.uid, GOOS: e.goos, Run: e.runCommand, Sleep: e.sleep}
	switch args[0] {
	case "install":
		return scheduleInstall(e, manager, args[1:])
	case "uninstall":
		return scheduleUninstall(e, manager, args[1:])
	case "status":
		return scheduleStatus(e, manager, args[1:])
	case "help", "--help", "-help", "-h":
		fmt.Fprint(e.stdout, scheduleUsage)
		return 0
	default:
		fmt.Fprintf(e.stderr, "granola-export: unknown schedule command %q\n\n%s", args[0], scheduleUsage)
		return 2
	}
}

func scheduleInstall(e *env, manager *schedule.Manager, args []string) int {
	flags := e.newFlagSet("schedule install", "granola-export schedule install --out <folder> [--at HH:MM] [--no-transcript]")
	out := flags.String("out", "", "folder to export into (required)")
	at := flags.String("at", "09:00", "time of day to sync, on the 24-hour clock")
	noTranscript := flags.Bool("no-transcript", false, "leave transcripts out")
	if code := e.parseFlags(flags, args); code >= 0 {
		return code
	}
	if *out == "" {
		fmt.Fprintln(e.stderr, "granola-export: --out <folder> is required")
		return 2
	}
	hour, minute, err := schedule.ParseTime(*at)
	if err != nil {
		fmt.Fprintf(e.stderr, "granola-export: --at: %v\n", err)
		return 2
	}
	if e.goos != "darwin" {
		return e.fail("%v", schedule.ErrUnsupported)
	}

	// launchd is given this exact path, so it has to stay where it is.
	binary, err := e.executable()
	if err == nil {
		binary, err = filepath.EvalSymlinks(binary)
	}
	if err != nil {
		return e.fail("cannot tell where granola-export is installed: %v", err)
	}
	if e.isTemporary(binary) {
		return e.fail("this copy of granola-export is in a temporary location (%s).\nMove it somewhere permanent, such as /usr/local/bin or ~/bin, and run the command again from there.", binary)
	}

	outDir, err := filepath.Abs(*out)
	if err != nil {
		return e.fail("%v", err)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return e.fail("cannot create the output folder: %v", err)
	}
	clock := fmt.Sprintf("%02d:%02d", hour, minute)
	if err := e.saveConfig(&config{Out: outDir, NoTranscript: *noTranscript, At: clock}); err != nil {
		return e.fail("cannot save settings: %v", err)
	}
	if err := manager.Install(schedule.Job{Binary: binary, Hour: hour, Minute: minute, LogPath: manager.LogPath()}); err != nil {
		return e.fail("%v", err)
	}

	fmt.Fprintf(e.stdout, `Daily sync installed.
  Folder:   %s
  Runs:     every day at %s, and when you log in. If the Mac is asleep at that time, it runs on wake.
  Program:  %s (the job points at this path; run "schedule install" again if you move it)
  Log:      %s
  Settings: %s
The first sync is starting now in the background.
`, outDir, clock, binary, manager.LogPath(), e.configPath())
	return 0
}

func scheduleUninstall(e *env, manager *schedule.Manager, args []string) int {
	flags := e.newFlagSet("schedule uninstall", "granola-export schedule uninstall")
	if code := e.parseFlags(flags, args); code >= 0 {
		return code
	}
	removed, err := manager.Uninstall()
	if err != nil {
		return e.fail("%v", err)
	}
	if removed {
		fmt.Fprintln(e.stdout, "Daily sync removed. Exported notes, settings and the log were left in place.")
	} else {
		fmt.Fprintln(e.stdout, "No daily sync was installed.")
	}
	return 0
}

func scheduleStatus(e *env, manager *schedule.Manager, args []string) int {
	flags := e.newFlagSet("schedule status", "granola-export schedule status")
	if code := e.parseFlags(flags, args); code >= 0 {
		return code
	}
	status, err := manager.Status()
	if err != nil {
		return e.fail("%v", err)
	}
	switch {
	case status.Installed && status.Loaded:
		fmt.Fprintln(e.stdout, "Daily sync: installed and loaded")
	case status.Installed:
		fmt.Fprintln(e.stdout, `Daily sync: installed, but launchd has not loaded it (it loads at next login, or run "schedule install" again)`)
	case status.Loaded:
		fmt.Fprintln(e.stdout, `Daily sync: loaded in launchd, but its file is gone (run "schedule install" again, or "schedule uninstall")`)
	default:
		fmt.Fprintln(e.stdout, "Daily sync: not installed")
	}

	cfg, err := e.loadConfig()
	if err != nil {
		return e.fail("%v", err)
	}
	if cfg == nil {
		return 0
	}
	if status.Installed && cfg.At != "" {
		fmt.Fprintf(e.stdout, "  Runs:      every day at %s, and when you log in\n", cfg.At)
	}
	fmt.Fprintf(e.stdout, "  Folder:    %s\n", cfg.Out)
	last, err := vaultsync.ReadLastSync(cfg.Out)
	switch {
	case err != nil:
		fmt.Fprintf(e.stdout, "  Last sync: unknown (%v)\n", err)
	case last == nil:
		fmt.Fprintln(e.stdout, "  Last sync: none has finished yet")
	default:
		fmt.Fprintf(e.stdout, "  Last sync: %s from %s: %d created, %d updated, %d unchanged, %d left alone as edited, %d failed\n",
			last.Time.In(e.loc).Format(timestampLayout), sourceLabel(last.Source), last.Created, last.Updated, last.Unchanged, last.Skipped, last.Failed)
	}
	fmt.Fprintf(e.stdout, "  Log:       %s\n", manager.LogPath())
	return 0
}

func sourceLabel(source string) string {
	if source == "cache" {
		return "Granola's local cache"
	}
	return "Granola's login"
}
