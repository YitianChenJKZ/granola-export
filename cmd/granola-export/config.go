package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// config holds the settings "schedule install" saves for the daily job.
// "sync" falls back to them for anything not given on the command line.
type config struct {
	Out          string `json:"out"`
	NoTranscript bool   `json:"no_transcript"`
	At           string `json:"at"`
}

func (e *env) configPath() string {
	return filepath.Join(e.configDir, "config.json")
}

// loadConfig returns nil if no settings have been saved.
func (e *env) loadConfig() (*config, error) {
	data, err := os.ReadFile(e.configPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON: %v", e.configPath(), err)
	}
	return &c, nil
}

func (e *env) saveConfig(c *config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(e.configDir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(e.configPath(), append(data, '\n'), 0o644)
}

func (e *env) newFlagSet(name, synopsis string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(e.stderr)
	flags.Usage = func() {
		fmt.Fprintf(e.stderr, "Usage: %s\n", synopsis)
		first := true
		flags.VisitAll(func(f *flag.Flag) {
			if first {
				fmt.Fprint(e.stderr, "\nFlags:\n")
				first = false
			}
			value, usage := flag.UnquoteUsage(f)
			line := "  --" + f.Name
			if value != "" {
				line += " " + value
			}
			if f.DefValue != "" && f.DefValue != "false" {
				usage += fmt.Sprintf(" (default %s)", f.DefValue)
			}
			fmt.Fprintf(e.stderr, "%s\n        %s\n", line, usage)
		})
	}
	return flags
}

// parseFlags returns the exit code to stop with, or -1 to carry on.
func (e *env) parseFlags(flags *flag.FlagSet, args []string) int {
	switch err := flags.Parse(args); {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(e.stderr, "granola-export: unexpected argument %q\n\n", flags.Arg(0))
		flags.Usage()
		return 2
	}
	return -1
}

func flagWasSet(flags *flag.FlagSet, name string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func (e *env) fail(format string, args ...any) int {
	fmt.Fprintf(e.stderr, "granola-export: "+format+"\n", args...)
	return 1
}
