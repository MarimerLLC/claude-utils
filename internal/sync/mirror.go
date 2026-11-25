// Package sync contains the two-way file mirror between Claude's authoritative
// memory tree (~/.claude/projects/<hash>/memory/) and the sync repo's
// work-tree (~/.claudesync/projects/<key>/memory/), plus the daemon loop
// that keeps them in sync via git.
package sync

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/MarimerLLC/claude-utils/internal/merge"
	"github.com/MarimerLLC/claude-utils/internal/project"
)

// Roots names a pair of (Claude authoritative, mirror) parents.
//
// For example:
//
//	Claude: C:\Users\rocky\.claude\projects
//	Mirror: C:\Users\rocky\.claudesync\projects
//
// Claude child dirs are named by Claude's dash-encoding of the absolute project
// path (localHash); mirror child dirs are named by the project's machine-
// independent fingerprint key, so the same repo lines up across machines that
// check it out at different paths. The project.Index maps between the two.
type Roots struct {
	Claude string // ~/.claude/projects
	Mirror string // ~/.claudesync/projects
}

// SyncReport is returned by reconciliation operations for diagnostics.
type SyncReport struct {
	CopiedToMirror    []string // relative paths copied Claude → mirror
	CopiedToClaude    []string // relative paths copied mirror → Claude
	Merged            []string // MEMORY.md paths semantically merged
	BackedUp          []string // mirror copies preserved as .from-remote-* on collision
	RemovedFromMirror []string // user-deleted files removed from mirror (manifest-detected)
	RemovedFromClaude []string // files removed from sibling checkouts of a deleted-in-one project
	PrunedLegacy      []string // legacy path-keyed mirror files removed after import
	Redirected        []string // legacy dirs given a moved-to redirect when pruned
	Errors            []string // per-project failures that were skipped (AdoptRekeyed)
}

// ErrNeedsReconcile is returned by the incremental helpers (CopyToMirror,
// RemoveFromMirror) when a change can't safely be applied on its own and needs
// a full Reconcile, which merges or keeps backups instead of overwriting.
// Nothing is written. That covers:
//   - a project not in the index yet — newly opened here (its mirror key may
//     already hold memories from another PC that a blind copy would clobber),
//     or still waiting for Claude to record its cwd;
//   - a sibling checkout of the same project whose copy differs from the
//     mirror — it has its own unsynced edit, which a fan-out would destroy.
var ErrNeedsReconcile = errors.New("change needs a full reconcile")

// Reconcile performs a bidirectional sync between the two trees, using the
// manifest to distinguish "user deleted this" from "this is new from
// another PC." Pass a nil or empty manifest to disable delete detection
// (the safe behavior for first-ever sync, since with no prior state we
// can't know what to delete). Pass a nil index to build one from the Claude
// tree.
//
// The Claude side stays keyed by localHash (Claude's dash-encoded dir name);
// the mirror side is keyed by the project's machine-independent fingerprint,
// resolved through idx. Each key is reconciled once, across every local project
// that maps to it (several when one repo is checked out more than once on this
// machine, e.g. as worktrees), so all of them and the mirror end up identical.
// Only projects present locally are reconciled — a fingerprint-keyed mirror dir
// with no local project (opened only on another PC) is left pending and
// materializes when that project is first opened here.
//
// Decisions for each file, folding each local copy into the mirror's:
//
//   - all copies identical → no-op
//   - copies differ → MEMORY.md is merged via the section-block merger; for
//     other files the Claude copy wins and the version it displaces is kept in
//     the mirror as <name>.from-remote-<random>
//   - a local copy missing → it receives the result
//   - missing in a local that had it at last sync (manifest), and no other
//     copy edited since → user deleted it; remove it from the mirror (so the
//     next git push propagates it) and from any sibling checkouts
//
// This function is used during `init` and as a periodic safety net on the
// pull tick. The watcher-driven sync loop also handles incremental events
// directly via CopyToMirror / RemoveFromMirror.
func Reconcile(r Roots, idx *project.Index, manifest *Manifest) (SyncReport, error) {
	rep := SyncReport{}
	if manifest == nil {
		manifest = NewManifest()
	}
	if idx == nil {
		var err error
		idx, err = project.Build(r.Claude, nil)
		if err != nil {
			return rep, err
		}
	}

	groups := map[string][]string{} // key → local project dirs
	for localHash, entry := range idx.All() {
		groups[entry.Key] = append(groups[entry.Key], localHash)
	}

	// A mirror-only dir with a legacy path key (== a localHash) still
	// materializes into Claude, so identical-path / non-git projects keep
	// syncing. Skipped:
	//   - fingerprint keys: can't be reversed to a path, so they wait;
	//   - names this PC's index knows under a different key: the legacy dir of
	//     a now-fingerprinted project. MigrateLayout imports it; writing to it
	//     would revive it as a second, permanently diverging copy;
	//   - names of local projects missing from the index: still pending a cwd,
	//     their key isn't known yet;
	//   - pruned legacy dirs that only hold a redirect (FollowRedirects handles
	//     any local project they belong to).
	mirrorEntries, err := os.ReadDir(r.Mirror)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return rep, fmt.Errorf("read mirror %s: %w", r.Mirror, err)
	}
	for _, e := range mirrorEntries {
		name := e.Name()
		if !e.IsDir() || project.IsFingerprintKey(name) || hasRedirect(filepath.Join(r.Mirror, name)) {
			continue
		}
		if _, ok := groups[name]; ok {
			continue
		}
		if _, ok := idx.Get(name); ok {
			continue
		}
		if _, err := os.Stat(filepath.Join(r.Claude, name)); err == nil {
			continue
		}
		groups[name] = []string{name}
		idx.Set(name, project.Entry{Key: name, Method: project.MethodPath})
	}

	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		locals := groups[key]
		sort.Strings(locals)
		if err := reconcileKey(r, key, locals, manifest, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

// reconcileKey reconciles one mirror key against all local projects mapping to it.
func reconcileKey(r Roots, key string, locals []string, manifest *Manifest, rep *SyncReport) error {
	mirrorMem := filepath.Join(r.Mirror, key, "memory")
	names := map[string]struct{}{}
	for _, localHash := range locals {
		claudeMem := filepath.Join(r.Claude, localHash, "memory")
		if err := listMemoryFiles(claudeMem, names); err != nil {
			return fmt.Errorf("read claude %s: %w", claudeMem, err)
		}
	}
	// A missing mirror dir contributes nothing; any other read error must stop
	// the pass rather than look like an empty dir whose files we'd overwrite.
	if err := listMemoryFiles(mirrorMem, names); err != nil {
		return fmt.Errorf("read mirror %s: %w", mirrorMem, err)
	}

	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, name := range sorted {
		if err := reconcileFile(r, mirrorMem, locals, name, manifest, rep); err != nil {
			return err
		}
	}
	return nil
}

// listMemoryFiles adds the syncable file names in dir to into. Dot-files and
// .from-remote-* collision backups are skipped. A missing dir is not an error.
func listMemoryFiles(dir string, into map[string]struct{}) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") || isCollisionBackup(e.Name()) {
			continue
		}
		into[e.Name()] = struct{}{}
	}
	return nil
}

func reconcileFile(r Roots, mirrorMem string, locals []string, name string, manifest *Manifest, rep *SyncReport) error {
	mp := filepath.Join(mirrorMem, name)
	mExists, mBytes := readIfExists(mp)

	type localCopy struct {
		path, rel string // rel is Claude-side (localHash/memory/name), as the manifest keys it
		exists    bool
		b         []byte
	}
	copies := make([]localCopy, len(locals))
	deletedBy := ""
	for i, localHash := range locals {
		p := filepath.Join(r.Claude, localHash, "memory", name)
		rel := filepath.ToSlash(filepath.Join(localHash, "memory", name))
		ex, b := readIfExists(p)
		copies[i] = localCopy{p, rel, ex, b}
		if !ex && deletedBy == "" && manifest.Has(rel) {
			deletedBy = rel
		}
	}

	if deletedBy != "" {
		edited := false
		for _, c := range copies {
			if c.exists && (!mExists || !bytes.Equal(c.b, mBytes)) {
				edited = true
				break
			}
		}
		if !edited {
			// Deleted on this PC and untouched everywhere else: delete it
			// everywhere, so the next git commit/push propagates the deletion.
			if mExists {
				if err := os.Remove(mp); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("remove mirror %s: %w", mp, err)
				}
				rep.RemovedFromMirror = append(rep.RemovedFromMirror, deletedBy)
			}
			for _, c := range copies {
				if !c.exists {
					continue
				}
				if err := os.Remove(c.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return fmt.Errorf("remove claude %s: %w", c.path, err)
				}
				rep.RemovedFromClaude = append(rep.RemovedFromClaude, c.rel)
			}
			return nil
		}
		// A copy edited since the last sync outranks the delete: keep it.
	}

	final, have := mBytes, mExists
	merged := false
	fromLocal := "" // which local copy `final` came from verbatim, for the report
	for _, c := range copies {
		if !c.exists {
			continue
		}
		switch {
		case !have:
			final, have, fromLocal = c.b, true, c.rel
		case bytes.Equal(c.b, final):
			// already matches
		case name == "MEMORY.md":
			m, _ := merge.Merge("", string(c.b), string(final))
			final, merged, fromLocal = []byte(m), true, ""
			rep.Merged = append(rep.Merged, c.rel)
		default:
			// Claude's copy wins; keep the version it displaces as a backup.
			backup := fmt.Sprintf("%s.from-remote-%s", mp, randSuffix())
			if err := writeFile(backup, final); err != nil {
				return err
			}
			rep.BackedUp = append(rep.BackedUp, c.rel)
			final, fromLocal = c.b, c.rel
		}
	}
	if !have {
		return nil
	}

	if !mExists || !bytes.Equal(final, mBytes) {
		if err := writeFile(mp, final); err != nil {
			return fmt.Errorf("write mirror %s: %w", mp, err)
		}
		if !merged {
			rep.CopiedToMirror = append(rep.CopiedToMirror, fromLocal)
		}
	}
	for _, c := range copies {
		if c.exists && bytes.Equal(c.b, final) {
			continue
		}
		if err := writeFile(c.path, final); err != nil {
			return fmt.Errorf("write claude %s: %w", c.path, err)
		}
		if !merged {
			rep.CopiedToClaude = append(rep.CopiedToClaude, c.rel)
		}
	}
	return nil
}

// CopyToMirror copies a single Claude-side file into the mirror under the
// project's fingerprint key, creating parent dirs as needed, and on to any
// other local checkouts of the same project so they never diverge. Used by the
// watcher loop on a file change. Writes are skipped when content already
// matches, which also stops the echo events those writes cause from looping.
//
// It returns ErrNeedsReconcile, writing nothing, when the project isn't indexed
// yet or a sibling checkout holds its own unsynced version of the file.
func CopyToMirror(r Roots, idx *project.Index, localHash, name string) error {
	entry, ok := idx.Get(localHash)
	if !ok {
		return ErrNeedsReconcile
	}
	b, err := os.ReadFile(filepath.Join(r.Claude, localHash, "memory", name))
	if err != nil {
		return err
	}
	dst := filepath.Join(r.Mirror, entry.Key, "memory", name)
	siblings, ok := siblingsInSync(r, idx, entry.Key, localHash, name)
	if !ok {
		return ErrNeedsReconcile
	}
	if _, err := writeIfChanged(dst, b); err != nil {
		return err
	}
	for _, p := range siblings {
		if _, err := writeIfChanged(p, b); err != nil {
			return err
		}
	}
	return nil
}

// siblingsInSync returns the file's paths in the other local checkouts mapping
// to key, and whether every one of them still matches the mirror's current copy
// (same content, or both absent) — i.e. none has an edit of its own that an
// incremental copy or delete would discard.
func siblingsInSync(r Roots, idx *project.Index, key, localHash, name string) ([]string, bool) {
	mExists, mBytes := readIfExists(filepath.Join(r.Mirror, key, "memory", name))
	var paths []string
	for _, other := range idx.LocalsFor(key) {
		if other == localHash {
			continue
		}
		p := filepath.Join(r.Claude, other, "memory", name)
		ex, b := readIfExists(p)
		if ex != mExists || (ex && !bytes.Equal(b, mBytes)) {
			return nil, false
		}
		paths = append(paths, p)
	}
	return paths, true
}

// CopyToClaude copies a single mirror-side file (addressed by fingerprint key)
// back into every local project that maps to that key, returning how many local
// projects it targeted. A key with no local project is skipped (0) — it
// materializes later when the project is first opened here. Used after a
// `git pull` brings in remote changes.
func CopyToClaude(r Roots, idx *project.Index, key, name string) (int, error) {
	locals := idx.LocalsFor(key)
	if len(locals) == 0 {
		return 0, nil
	}
	b, err := os.ReadFile(filepath.Join(r.Mirror, key, "memory", name))
	if err != nil {
		return 0, err
	}
	for _, localHash := range locals {
		if _, err := writeIfChanged(filepath.Join(r.Claude, localHash, "memory", name), b); err != nil {
			return 0, err
		}
	}
	return len(locals), nil
}

// RemoveFromMirror handles a Claude-side delete: remove the corresponding mirror
// file (under the project's fingerprint key) so the next commit propagates the
// deletion, plus the copies in any other local checkouts of the project.
//
// Remove/Rename events also fire when a file is atomically replaced; if the
// file is still there it wasn't deleted, so it is copied instead. Like
// CopyToMirror it returns ErrNeedsReconcile when the project isn't indexed or a
// sibling checkout has edited the file — Reconcile then lets that edit outrank
// the delete instead of discarding it.
func RemoveFromMirror(r Roots, idx *project.Index, localHash, name string) error {
	entry, ok := idx.Get(localHash)
	if !ok {
		return ErrNeedsReconcile
	}
	if _, err := os.Stat(filepath.Join(r.Claude, localHash, "memory", name)); err == nil {
		return CopyToMirror(r, idx, localHash, name)
	}
	siblings, ok := siblingsInSync(r, idx, entry.Key, localHash, name)
	if !ok {
		return ErrNeedsReconcile
	}
	targets := append([]string{filepath.Join(r.Mirror, entry.Key, "memory", name)}, siblings...)
	for _, t := range targets {
		if err := os.Remove(t); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func readIfExists(p string) (bool, []byte) {
	b, err := os.ReadFile(p)
	if err != nil {
		return false, nil
	}
	return true, b
}

// writeFile creates path's parent dir if needed, then writes atomically.
func writeFile(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return writeAtomic(path, content)
}

// writeIfChanged writes content to path unless it already holds exactly that,
// reporting whether it wrote.
func writeIfChanged(path string, content []byte) (bool, error) {
	if ok, cur := readIfExists(path); ok && bytes.Equal(cur, content) {
		return false, nil
	}
	return true, writeFile(path, content)
}

// writeAtomic writes content to a unique sibling .tmp file then renames it
// over the destination. This avoids exposing partial content to a reader
// (e.g., Claude reading a memory while we're mid-write).
func writeAtomic(path string, content []byte) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, fmt.Sprintf(".%s.%s.tmp", filepath.Base(path), randSuffix()))
	if err := os.WriteFile(tmp, content, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func randSuffix() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// fall back to time
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
