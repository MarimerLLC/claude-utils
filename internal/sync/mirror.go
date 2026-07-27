// Package sync contains the two-way file mirror between Claude's authoritative
// memory tree (~/.claude/projects/<hash>/memory/) and the sync repo's
// work-tree (~/.claudesync/projects/<hash>/memory/), plus the daemon loop
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
}

// Reconcile performs a bidirectional sync between the two trees, using the
// manifest to distinguish "user deleted this" from "this is new from
// another PC." Pass a nil or empty manifest to disable delete detection
// (the safe behavior for first-ever sync, since with no prior state we
// can't know what to delete). Pass a nil index to build one from the Claude
// tree.
//
// The Claude side stays keyed by localHash (Claude's dash-encoded dir name);
// the mirror side is keyed by the project's machine-independent fingerprint,
// resolved through idx. Only projects present locally are reconciled — a mirror
// key with no local project (opened only on another PC) is left pending and
// materializes when that project is first opened here.
//
// Decisions for each file path:
//
//   - Claude has, mirror has, identical → no-op
//   - Claude has, mirror has, differs    → MEMORY.md is merged via the
//     section-block merger; other files keep Claude and back the mirror
//     copy up as <name>.from-remote-<random>
//   - Claude has, mirror missing         → copy Claude → mirror
//   - Claude missing, mirror has, in manifest    → user deleted; remove
//     from mirror so the next git push propagates it
//   - Claude missing, mirror has, NOT in manifest → new from another PC;
//     copy mirror → Claude
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
		idx, err = project.Build(r.Claude)
		if err != nil {
			return rep, err
		}
	}

	// Reconcile every local project against its key, plus any mirror-only dir
	// whose key is a legacy path key (== a localHash) so inbound files for
	// identical-path / non-git projects still materialize into Claude. A
	// fingerprint-keyed mirror dir with no local project can't be reversed to a
	// path, so it waits (materializes when the project is first opened here).
	type pair struct{ localHash, key string }
	var pairs []pair
	seenKey := map[string]bool{}
	for localHash, entry := range idx.All() {
		pairs = append(pairs, pair{localHash, entry.Key})
		seenKey[entry.Key] = true
	}
	if mirrorEntries, err := os.ReadDir(r.Mirror); err == nil {
		for _, e := range mirrorEntries {
			if !e.IsDir() || seenKey[e.Name()] || project.IsFingerprintKey(e.Name()) {
				continue
			}
			pairs = append(pairs, pair{e.Name(), e.Name()})
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return rep, fmt.Errorf("read mirror %s: %w", r.Mirror, err)
	}

	for _, p := range pairs {
		localHash := p.localHash
		claudeMem := filepath.Join(r.Claude, localHash, "memory")
		mirrorMem := filepath.Join(r.Mirror, p.key, "memory")
		if err := os.MkdirAll(mirrorMem, 0700); err != nil {
			return rep, err
		}

		files := map[string]struct{}{}
		// Walk Claude side
		if entries, err := os.ReadDir(claudeMem); err == nil {
			for _, e := range entries {
				if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				files[e.Name()] = struct{}{}
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return rep, fmt.Errorf("read claude %s: %w", claudeMem, err)
		}
		// Walk mirror side
		if entries, err := os.ReadDir(mirrorMem); err == nil {
			for _, e := range entries {
				if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
					continue
				}
				if strings.Contains(e.Name(), ".from-remote-") {
					continue
				}
				files[e.Name()] = struct{}{}
			}
		}

		for name := range files {
			cp := filepath.Join(claudeMem, name)
			mp := filepath.Join(mirrorMem, name)
			// Manifest keys are Claude-side (localHash), independent of the mirror
			// key, so fingerprinting doesn't disturb delete detection.
			rel := filepath.ToSlash(filepath.Join(localHash, "memory", name))

			cExists, cBytes := readIfExists(cp)
			mExists, mBytes := readIfExists(mp)
			wasInManifest := manifest.Has(rel)

			switch {
			case cExists && !mExists:
				if err := writeAtomic(mp, cBytes); err != nil {
					return rep, fmt.Errorf("write mirror %s: %w", mp, err)
				}
				rep.CopiedToMirror = append(rep.CopiedToMirror, rel)

			case !cExists && mExists && wasInManifest:
				// User deleted on this PC. Remove from mirror so the next
				// git commit/push propagates the deletion.
				if err := os.Remove(mp); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return rep, fmt.Errorf("remove mirror %s: %w", mp, err)
				}
				rep.RemovedFromMirror = append(rep.RemovedFromMirror, rel)

			case !cExists && mExists:
				// Not in manifest → new from another PC.
				if err := os.MkdirAll(claudeMem, 0700); err != nil {
					return rep, err
				}
				if err := writeAtomic(cp, mBytes); err != nil {
					return rep, fmt.Errorf("write claude %s: %w", cp, err)
				}
				rep.CopiedToClaude = append(rep.CopiedToClaude, rel)

			case cExists && mExists && bytes.Equal(cBytes, mBytes):
				// already matched — nothing to do

			case cExists && mExists && !bytes.Equal(cBytes, mBytes):
				if name == "MEMORY.md" {
					merged, _ := merge.Merge("", string(cBytes), string(mBytes))
					if err := writeAtomic(cp, []byte(merged)); err != nil {
						return rep, err
					}
					if err := writeAtomic(mp, []byte(merged)); err != nil {
						return rep, err
					}
					rep.Merged = append(rep.Merged, rel)
				} else {
					backup := fmt.Sprintf("%s.from-remote-%s", mp, randSuffix())
					if err := writeAtomic(backup, mBytes); err != nil {
						return rep, err
					}
					if err := writeAtomic(mp, cBytes); err != nil {
						return rep, err
					}
					rep.CopiedToMirror = append(rep.CopiedToMirror, rel)
					rep.BackedUp = append(rep.BackedUp, rel)
				}
			}
		}
	}
	return rep, nil
}

// CopyToMirror copies a single Claude-side file into the mirror under the
// project's fingerprint key, creating parent dirs as needed. Used by the watcher
// loop on a file change.
func CopyToMirror(r Roots, idx *project.Index, localHash, name string) error {
	key := idx.KeyFor(r.Claude, localHash)
	src := filepath.Join(r.Claude, localHash, "memory", name)
	dst := filepath.Join(r.Mirror, key, "memory", name)
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	return writeAtomic(dst, b)
}

// CopyToClaude copies a single mirror-side file (addressed by fingerprint key)
// back into every local project that maps to that key. A key with no local
// project is skipped — it materializes later when the project is first opened
// here. Used after a `git pull` brings in remote changes.
func CopyToClaude(r Roots, idx *project.Index, key, name string) error {
	src := filepath.Join(r.Mirror, key, "memory", name)
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	for _, localHash := range idx.LocalsFor(key) {
		dst := filepath.Join(r.Claude, localHash, "memory", name)
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		if err := writeAtomic(dst, b); err != nil {
			return err
		}
	}
	return nil
}

// RemoveFromMirror handles a Claude-side delete: remove the corresponding mirror
// file (under the project's fingerprint key) so the next commit propagates the
// deletion.
func RemoveFromMirror(r Roots, idx *project.Index, localHash, name string) error {
	key := idx.KeyFor(r.Claude, localHash)
	target := filepath.Join(r.Mirror, key, "memory", name)
	err := os.Remove(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// MigrateLayout moves legacy path-keyed mirror directories to their fingerprint
// keys, so a project previously split by path (a separate dir per machine)
// collapses to one key. When the target key dir already exists — because another
// machine already migrated and pushed it, or a prior run did — the two are
// unioned: MEMORY.md via the semantic merge driver, other files copied when
// absent, and genuine conflicts preserved as <name>.from-remote-<random>.
//
// Idempotent: once a project's mirror dir is its fingerprint key there is no
// legacy dir left to move. Projects that can't be fingerprinted keep their path
// key and are left untouched.
func MigrateLayout(r Roots, idx *project.Index) (SyncReport, error) {
	rep := SyncReport{}
	for localHash, entry := range idx.All() {
		if entry.Key == localHash {
			continue // path fallback — already at its final key
		}
		oldProj := filepath.Join(r.Mirror, localHash)
		oldMem := filepath.Join(oldProj, "memory")
		if _, err := os.Stat(oldMem); err != nil {
			continue // nothing legacy to migrate for this project
		}
		newMem := filepath.Join(r.Mirror, entry.Key, "memory")
		if err := os.MkdirAll(newMem, 0700); err != nil {
			return rep, err
		}

		entries, err := os.ReadDir(oldMem)
		if err != nil {
			return rep, err
		}
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			name := e.Name()
			rel := filepath.ToSlash(filepath.Join(entry.Key, "memory", name))
			oldBytes, err := os.ReadFile(filepath.Join(oldMem, name))
			if err != nil {
				return rep, err
			}
			newPath := filepath.Join(newMem, name)
			newExists, newBytes := readIfExists(newPath)
			switch {
			case !newExists:
				if err := writeAtomic(newPath, oldBytes); err != nil {
					return rep, err
				}
				rep.CopiedToMirror = append(rep.CopiedToMirror, rel)
			case bytes.Equal(oldBytes, newBytes):
				// identical — nothing to do
			case name == "MEMORY.md":
				merged, _ := merge.Merge("", string(newBytes), string(oldBytes))
				if err := writeAtomic(newPath, []byte(merged)); err != nil {
					return rep, err
				}
				rep.Merged = append(rep.Merged, rel)
			default:
				backup := fmt.Sprintf("%s.from-remote-%s", newPath, randSuffix())
				if err := writeAtomic(backup, oldBytes); err != nil {
					return rep, err
				}
				rep.BackedUp = append(rep.BackedUp, rel)
			}
		}

		if err := os.RemoveAll(oldProj); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func readIfExists(p string) (bool, []byte) {
	b, err := os.ReadFile(p)
	if err != nil {
		return false, nil
	}
	return true, b
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
