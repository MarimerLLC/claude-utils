package project

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
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
	Path   string `json:"path,omitempty"`  // resolved real path, for diagnostics
	Stamp  string `json:"stamp,omitempty"` // TranscriptStamp when last resolved
}

type indexJSON struct {
	Version int              `json:"version"`
	Entries map[string]Entry `json:"entries"`
}

const (
	indexStateDir = ".state"
	indexFileName = "index.json"
	// indexVersion 2 folds the in-repo subdirectory into keys; a version-1
	// file's keys are not comparable, so Load ignores it.
	indexVersion = 2

	// pendingWindow is how long after a project's newest transcript was written
	// we keep waiting for Claude to record a cwd in it before giving up and using
	// the path key.
	pendingWindow = 10 * time.Minute
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
//
// prev (may be nil) is the previous index — the in-memory one, or the one saved
// on disk. It keeps keys stable and rebuilds cheap:
//   - a project whose transcripts haven't changed keeps its entry without
//     re-running git;
//   - a project that was fingerprinted before but whose checkout is now gone
//     (removed worktree, unmounted drive, transcripts aged out) keeps its last
//     key instead of re-keying to the path.
//
// A freshly created project whose transcript doesn't carry a cwd yet is left
// out entirely (it is picked up on a later build) rather than being pinned to
// the path key.
func Build(claudeProjectsDir string, prev *Index) (*Index, error) {
	idx := NewIndex()
	entries, err := os.ReadDir(claudeProjectsDir)
	if errors.Is(err, fs.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		// A dir named like a fingerprint key is not a Claude project: older
		// daemon versions materialized mirror dirs verbatim, so a pre-upgrade PC
		// may hold ~/.claude/projects/g-<hash> copies. Never treat them as
		// projects (they would join the real project's key as a phantom sibling).
		if !e.IsDir() || IsFingerprintKey(e.Name()) {
			continue
		}
		entry, pending := resolveEntry(claudeProjectsDir, e.Name(), prev)
		if pending {
			continue
		}
		idx.byLocal[e.Name()] = entry
	}
	return idx, nil
}

// resolveEntry computes one project's entry. pending reports a project that
// can't be keyed yet and must not be cached.
func resolveEntry(claudeProjectsDir, localHash string, prev *Index) (entry Entry, pending bool) {
	dir := filepath.Join(claudeProjectsDir, localHash)
	stamp := TranscriptStamp(dir)
	var old Entry
	hadOld := false
	if prev != nil {
		old, hadOld = prev.byLocal[localHash]
	}
	if hadOld && stamp != "" && old.Stamp == stamp && old.Method != MethodPath {
		// Nothing Claude records about this project changed. Path-keyed entries
		// are always re-resolved: decoding the folder name (below) can key them
		// even when their transcripts can't.
		return old, false
	}

	realPath, resolved := ResolvePath(dir)
	if !isDir(realPath) {
		// No transcript names the project's directory (a folder sync materialized
		// for a project never opened here), or the one it names is gone (a renamed
		// or moved checkout). The folder name itself encodes this machine's path,
		// so look for the directory it stands for. Every PC with the project at
		// that path then fingerprints it the same way, instead of this PC alone
		// staying on the path key — which another PC's pruneLegacyDirs would empty.
		if p, ok := DecodePath(localHash); ok {
			realPath, resolved = p, true
		}
	}
	key, method := Fingerprint(realPath, localHash)
	if method == MethodPath && hadOld && old.Method != MethodPath && !isDir(realPath) {
		// The checkout this project was fingerprinted from is gone or
		// unreachable. Re-keying to the path would republish its memories under
		// a second, per-machine mirror dir, so keep the last known key.
		old.Stamp = stamp
		return old, false
	}
	if !resolved && transcriptRecentlyWritten(dir, pendingWindow) {
		return Entry{}, true // Claude may not have written the cwd yet
	}
	return Entry{Key: key, Method: method, Path: realPath, Stamp: stamp}, false
}

func isDir(p string) bool {
	if p == "" {
		return false
	}
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// Set records an entry directly. Reconcile uses it for path-keyed projects it
// materializes from the mirror, so later events for them resolve from cache.
func (idx *Index) Set(localHash string, e Entry) { idx.byLocal[localHash] = e }

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
	if raw.Version != indexVersion {
		return idx, nil // keys from another scheme; rebuild from scratch
	}
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
