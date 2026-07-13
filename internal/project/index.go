package project

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// Index maps each local Claude project directory name (localHash) to the
// machine-independent key its memories sync under. It is per-PC state: localHash
// values encode this machine's paths, so the index never propagates across
// workstations (it lives under .state/, which is gitignored).
type Index struct {
	byLocal map[string]Entry
}

// Entry is one project's resolved identity.
type Entry struct {
	Key    string `json:"key"`
	Method Method `json:"method"`
	Path   string `json:"path,omitempty"` // resolved real path, for diagnostics
}

type indexJSON struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

const (
	indexStateDir = ".state"
	indexFileName = "index.json"
	indexVersion  = 1
)

// NewIndex returns an empty index.
func NewIndex() *Index { return &Index{byLocal: map[string]Entry{}} }

// IndexPath returns the absolute path of the index file inside syncDir.
func IndexPath(syncDir string) string {
	return filepath.Join(syncDir, indexStateDir, indexFileName)
}

// Build scans claudeProjectsDir and resolves every project directory to its key.
// Directories whose real path can't be resolved, or that aren't git repos, fall
// back to the path key (their own name).
func Build(claudeProjectsDir string) (*Index, error) {
	idx := NewIndex()
	entries, err := os.ReadDir(claudeProjectsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		localHash := e.Name()
		realPath, _ := ResolvePath(filepath.Join(claudeProjectsDir, localHash))
		key, method := Fingerprint(realPath, localHash)
		idx.byLocal[localHash] = Entry{Key: key, Method: method, Path: realPath}
	}
	return idx, nil
}

// KeyFor returns the mirror key for a local project directory, computing and
// caching it on a miss (so the daemon's incremental path doesn't need a full
// rebuild when a new project appears mid-session).
func (idx *Index) KeyFor(claudeProjectsDir, localHash string) string {
	if e, ok := idx.byLocal[localHash]; ok {
		return e.Key
	}
	realPath, _ := ResolvePath(filepath.Join(claudeProjectsDir, localHash))
	key, method := Fingerprint(realPath, localHash)
	idx.byLocal[localHash] = Entry{Key: key, Method: method, Path: realPath}
	return key
}

// LocalsFor returns the local project directories that map to key. Usually one,
// but a repo checked out at two paths on the same machine yields several — all of
// them should receive inbound memories for that project.
func (idx *Index) LocalsFor(key string) []string {
	var out []string
	for lh, e := range idx.byLocal {
		if e.Key == key {
			out = append(out, lh)
		}
	}
	sort.Strings(out)
	return out
}

// Get returns the entry for a local project directory, if known.
func (idx *Index) Get(localHash string) (Entry, bool) {
	e, ok := idx.byLocal[localHash]
	return e, ok
}

// All returns a copy of the localHash→Entry mapping.
func (idx *Index) All() map[string]Entry {
	out := make(map[string]Entry, len(idx.byLocal))
	for k, v := range idx.byLocal {
		out[k] = v
	}
	return out
}

// Load reads the index from syncDir. A missing file yields an empty index and no
// error, so first-run callers need no special-casing.
func Load(syncDir string) (*Index, error) {
	b, err := os.ReadFile(IndexPath(syncDir))
	if errors.Is(err, fs.ErrNotExist) {
		return NewIndex(), nil
	}
	if err != nil {
		return nil, err
	}
	var raw indexJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	idx := NewIndex()
	for lh, e := range raw.Entries {
		idx.byLocal[lh] = e
	}
	return idx, nil
}

// Save writes the index to syncDir as pretty-printed JSON.
func (idx *Index) Save(syncDir string) error {
	b, err := json.MarshalIndent(indexJSON{Version: indexVersion, Entries: idx.byLocal}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(IndexPath(syncDir)), 0700); err != nil {
		return err
	}
	return os.WriteFile(IndexPath(syncDir), b, 0600)
}
