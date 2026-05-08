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
)

// Roots names a pair of (Claude authoritative, mirror) parents.
//
// For example:
//
//	Claude: C:\Users\rocky\.claude\projects
//	Mirror: C:\Users\rocky\.claudesync\projects
//
// Each child directory under either root represents one project (named by
// Claude's hash of the absolute project path).
type Roots struct {
	Claude string // ~/.claude/projects
	Mirror string // ~/.claudesync/projects
}

// MemoryDirs lists every <hash>/memory directory found under either root.
func (r Roots) MemoryDirs() ([]string, error) {
	seen := map[string]struct{}{}
	for _, root := range []string{r.Claude, r.Mirror} {
		entries, err := os.ReadDir(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", root, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			seen[e.Name()] = struct{}{}
		}
	}
	dirs := make([]string, 0, len(seen))
	for hash := range seen {
		dirs = append(dirs, hash)
	}
	return dirs, nil
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
// can't know what to delete).
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
func Reconcile(r Roots, manifest *Manifest) (SyncReport, error) {
	rep := SyncReport{}
	if manifest == nil {
		manifest = NewManifest()
	}

	hashes, err := r.MemoryDirs()
	if err != nil {
		return rep, err
	}

	for _, hash := range hashes {
		claudeMem := filepath.Join(r.Claude, hash, "memory")
		mirrorMem := filepath.Join(r.Mirror, hash, "memory")
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
			rel := filepath.ToSlash(filepath.Join(hash, "memory", name))

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

// CopyToMirror copies a single Claude-side file into the mirror, creating
// parent dirs as needed. Used by the watcher loop on a file change.
func CopyToMirror(r Roots, hash, name string) error {
	src := filepath.Join(r.Claude, hash, "memory", name)
	dst := filepath.Join(r.Mirror, hash, "memory", name)
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	return writeAtomic(dst, b)
}

// CopyToClaude copies a single mirror-side file back into Claude's tree.
// Used after a `git pull` brings in remote changes.
func CopyToClaude(r Roots, hash, name string) error {
	src := filepath.Join(r.Mirror, hash, "memory", name)
	dst := filepath.Join(r.Claude, hash, "memory", name)
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
		return err
	}
	return writeAtomic(dst, b)
}

// RemoveFromMirror handles a Claude-side delete: remove the corresponding
// mirror file so the next commit propagates the deletion.
func RemoveFromMirror(r Roots, hash, name string) error {
	target := filepath.Join(r.Mirror, hash, "memory", name)
	err := os.Remove(target)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
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
