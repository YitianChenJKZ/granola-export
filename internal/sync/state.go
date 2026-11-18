package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

const (
	// StateDir sits inside the output folder. Obsidian hides dot-folders.
	StateDir     = ".granola-export"
	stateFile    = "state.json"
	stateVersion = 1
)

type state struct {
	Version  int               `json:"version"`
	LastSync *LastSync         `json:"last_sync,omitempty"`
	Notes    map[string]*entry `json:"notes"`
}

// LastSync summarises the most recent run that finished.
type LastSync struct {
	Time      time.Time `json:"time"`
	Source    string    `json:"source"`
	Created   int       `json:"created"`
	Updated   int       `json:"updated"`
	Unchanged int       `json:"unchanged"`
	Skipped   int       `json:"skipped_edited"`
	Failed    int       `json:"failed"`
}

// entry records what was last written for one note.
type entry struct {
	// Path is where the file was written, relative to the output folder.
	// The file is found by its granola_id property, so this is only a hint.
	Path string `json:"path,omitempty"`
	// SourceHash identifies the note content the file was rendered from.
	SourceHash string `json:"source_hash,omitempty"`
	// FileHash identifies the bytes written, to notice later edits.
	FileHash  string `json:"file_hash,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
	Source    string `json:"source"`
	// Empty marks a note that had nothing to export when last checked.
	Empty         bool `json:"empty,omitempty"`
	HasSummary    bool `json:"has_summary,omitempty"`
	HasTranscript bool `json:"has_transcript,omitempty"`
}

func statePath(outDir string) string {
	return filepath.Join(outDir, StateDir, stateFile)
}

func loadState(outDir string) (*state, error) {
	path := statePath(outDir)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &state{Version: stateVersion, Notes: map[string]*entry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("%s is damaged (%v); delete it to start over: files identical to a fresh export are picked up again, others are reported as edited", path, err)
	}
	if st.Version > stateVersion {
		return nil, fmt.Errorf("%s was written by a newer version of granola-export", path)
	}
	if st.Notes == nil {
		st.Notes = map[string]*entry{}
	}
	for id, e := range st.Notes {
		if e == nil {
			delete(st.Notes, id)
		}
	}
	st.Version = stateVersion
	return &st, nil
}

func saveState(outDir string, st *state) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(outDir, StateDir), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(statePath(outDir), append(data, '\n'), 0o644)
}

// ReadLastSync returns the summary of the last finished sync into outDir, or
// nil if there has not been one.
func ReadLastSync(outDir string) (*LastSync, error) {
	st, err := loadState(outDir)
	if err != nil {
		return nil, err
	}
	return st.LastSync, nil
}

func hash(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:])
}

// writeFileAtomic replaces path in one step, so a reader (Obsidian, a file
// sync service) never sees a half-written file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".granola-export-*.tmp")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Chmod(mode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}
