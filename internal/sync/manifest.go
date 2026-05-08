package sync

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Manifest records which Claude-side memory files were present as of the
// last successful sync. Reconcile reads it to distinguish "user deleted
// this on this PC" (mirror has it, manifest had it, Claude lacks it) from
// "this file is new from another PC" (mirror has it, manifest didn't,
// Claude lacks it). Without the manifest those two cases are
// indistinguishable, and Reconcile would silently un-do user deletes by
// re-copying from mirror to Claude.
//
// The manifest lives at ~/.claudesync/.state/manifest.json and is per-PC:
// .state/ is added to .gitignore so the manifest never propagates across
// workstations.
type Manifest struct {
	files map[string]struct{}
}

type manifestJSON struct {
	Version int      `json:"version"`
	Files   []string `json:"files"`
}

const (
	manifestStateDir = ".state"
	manifestFileName = "manifest.json"
	manifestVersion  = 1
)

// NewManifest returns an empty manifest (no known prior sync state).
func NewManifest() *Manifest {
	return &Manifest{files: map[string]struct{}{}}
}

// ManifestPath returns the absolute path of the manifest file inside syncDir.
func ManifestPath(syncDir string) string {
	return filepath.Join(syncDir, manifestStateDir, manifestFileName)
}

// LoadManifest reads the manifest from disk. If the file does not exist
// the returned Manifest is empty and no error is reported, so first-run
// callers can proceed without special-casing.
func LoadManifest(syncDir string) (*Manifest, error) {
	b, err := os.ReadFile(ManifestPath(syncDir))
	if errors.Is(err, fs.ErrNotExist) {
		return NewManifest(), nil
	}
	if err != nil {
		return nil, err
	}
	var raw manifestJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	m := NewManifest()
	for _, p := range raw.Files {
		m.files[p] = struct{}{}
	}
	return m, nil
}

// Save writes the manifest to disk as pretty-printed JSON with paths sorted
// for stable diffs (helpful when reading by hand).
func (m *Manifest) Save(syncDir string) error {
	paths := make([]string, 0, len(m.files))
	for p := range m.files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	b, err := json.MarshalIndent(manifestJSON{Version: manifestVersion, Files: paths}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(ManifestPath(syncDir)), 0700); err != nil {
		return err
	}
	return os.WriteFile(ManifestPath(syncDir), b, 0600)
}

// Has reports whether the given relative path (e.g. "<hash>/memory/MEMORY.md",
// always forward-slashed) was present at last sync.
func (m *Manifest) Has(rel string) bool {
	_, ok := m.files[rel]
	return ok
}

// Len returns the number of files tracked.
func (m *Manifest) Len() int { return len(m.files) }

// Add records a file as present.
func (m *Manifest) Add(rel string) { m.files[rel] = struct{}{} }

// BuildFromClaudeTree walks claudeRoot and returns a manifest with every
// existing memory file. Used to refresh the manifest after each successful
// sync and to bootstrap on first run.
func BuildFromClaudeTree(claudeRoot string) (*Manifest, error) {
	m := NewManifest()
	entries, err := os.ReadDir(claudeRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		memDir := filepath.Join(claudeRoot, e.Name(), "memory")
		files, err := os.ReadDir(memDir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if f.IsDir() || strings.HasPrefix(f.Name(), ".") {
				continue
			}
			rel := filepath.ToSlash(filepath.Join(e.Name(), "memory", f.Name()))
			m.files[rel] = struct{}{}
		}
	}
	return m, nil
}
