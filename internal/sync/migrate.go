package sync

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/merge"
	"github.com/MarimerLLC/claude-utils/internal/project"
)

// MigrateOptions tunes MigrateLayout.
type MigrateOptions struct {
	// State records which legacy file versions this PC has already imported,
	// so an unchanged legacy file is not imported again — re-merging a
	// MEMORY.md would otherwise resurrect sections deleted since. nil imports
	// everything (first-run semantics).
	State *MigrationState

	// Prior, if set, reports whether content is a version the fingerprint-keyed
	// file at rel ("<key>/memory/<name>") has held at some point in the sync
	// repo's history. Such a legacy copy is stale — another upgraded PC already
	// imported it and the file has moved on (or been deleted) since — so it is
	// skipped rather than merged back in or saved as a collision backup.
	Prior PriorVersionFunc

	// Prune deletes legacy files once imported (and their dirs once empty). Only
	// safe once every PC syncing this repo runs a fingerprint-aware version: an
	// older version reads the deletion as "user deleted these memories" and
	// removes its local copies.
	Prune bool
}

// PriorVersionFunc: see MigrateOptions.Prior.
type PriorVersionFunc func(rel string, content []byte) bool

// MigrateLayout imports legacy path-keyed mirror directories into their
// fingerprint keys, so a project previously split by path (a separate dir per
// machine) collapses to one key. When the key dir already holds the file —
// another machine imported it, or this project has been syncing under the key —
// the two are unioned: MEMORY.md via the semantic merge driver, other files
// copied when absent, and genuine conflicts preserved as
// <name>.from-remote-<random>.
//
// Legacy dirs are left in place unless opt.Prune is set: PCs still running a
// pre-fingerprint version keep reading and writing them, and deleting them
// would make those PCs delete the same memories locally (and could conflict
// with their pending edits). Run on every pull tick, it keeps importing edits
// such PCs make; Reconcile never writes to a legacy dir.
//
// Projects that can't be fingerprinted keep their path key and are left
// untouched.
func MigrateLayout(r Roots, idx *project.Index, opt MigrateOptions) (SyncReport, error) {
	rep := SyncReport{}
	all := idx.All()
	locals := make([]string, 0, len(all))
	for lh := range all {
		locals = append(locals, lh)
	}
	sort.Strings(locals)

	for _, localHash := range locals {
		entry := all[localHash]
		if entry.Key == localHash {
			continue // path fallback — already at its final key
		}
		oldProj := filepath.Join(r.Mirror, localHash)
		oldMem := filepath.Join(oldProj, "memory")
		entries, err := os.ReadDir(oldMem)
		if errors.Is(err, fs.ErrNotExist) {
			continue // nothing legacy to migrate for this project
		}
		if err != nil {
			return rep, err
		}
		newMem := filepath.Join(r.Mirror, entry.Key, "memory")

		var imported []string
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			name := e.Name()
			legacyRel := filepath.ToSlash(filepath.Join(localHash, "memory", name))
			rel := filepath.ToSlash(filepath.Join(entry.Key, "memory", name))
			oldBytes, err := os.ReadFile(filepath.Join(oldMem, name))
			if err != nil {
				return rep, err
			}
			sum := contentSum(oldBytes)
			if opt.State != nil && opt.State.Imported(legacyRel) == sum {
				imported = append(imported, name)
				continue // this exact version was imported before
			}

			newPath := filepath.Join(newMem, name)
			newExists, newBytes := readIfExists(newPath)
			switch {
			case isCollisionBackup(name):
				// A .from-remote-* recovery copy is not a memory: never merge or
				// re-back it up. Carry it over verbatim (names are unique) so it
				// stays recoverable under the key — pruning removes the legacy
				// copy only after this one exists.
				if !newExists {
					if err := writeFile(newPath, oldBytes); err != nil {
						return rep, err
					}
				}
			case newExists && bytes.Equal(oldBytes, newBytes):
				// identical — nothing to do
			case opt.Prior != nil && opt.Prior(rel, oldBytes):
				// stale: the key's file already held this version and moved on
			case !newExists:
				if err := writeFile(newPath, oldBytes); err != nil {
					return rep, err
				}
				rep.CopiedToMirror = append(rep.CopiedToMirror, rel)
			case name == "MEMORY.md":
				merged, _ := merge.Merge("", string(newBytes), string(oldBytes))
				if err := writeFile(newPath, []byte(merged)); err != nil {
					return rep, err
				}
				rep.Merged = append(rep.Merged, rel)
			default:
				backup := fmt.Sprintf("%s.from-remote-%s", newPath, randSuffix())
				if err := writeFile(backup, oldBytes); err != nil {
					return rep, err
				}
				rep.BackedUp = append(rep.BackedUp, rel)
			}
			if opt.State != nil {
				opt.State.Record(legacyRel, sum)
			}
			imported = append(imported, name)
		}

		if opt.Prune {
			// Remove only what was imported; anything skipped above (subdirs,
			// dot-files) stays, and with it the dir.
			for _, name := range imported {
				if err := os.Remove(filepath.Join(oldMem, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return rep, err
				}
				legacyRel := filepath.ToSlash(filepath.Join(localHash, "memory", name))
				rep.PrunedLegacy = append(rep.PrunedLegacy, legacyRel)
				if opt.State != nil {
					opt.State.Forget(legacyRel)
				}
			}
			_ = os.Remove(oldMem)  // fails (kept) unless empty
			_ = os.Remove(oldProj) // likewise
		}
	}
	return rep, nil
}

// isCollisionBackup reports whether name is a .from-remote-* backup written on
// a collision (Reconcile and the mirror walk skip these as non-memories).
func isCollisionBackup(name string) bool { return strings.Contains(name, ".from-remote-") }

func contentSum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// PriorVersionIn returns a PriorVersionFunc backed by the sync repo's history:
// it reports whether the content's blob ever appeared at projects/<rel> on any
// ref.
func PriorVersionIn(repo *gitwt.Repo) PriorVersionFunc {
	format := "sha1"
	if out, err := repo.Run("rev-parse", "--show-object-format"); err == nil {
		if f := strings.TrimSpace(out); f != "" {
			format = f
		}
	}
	return func(rel string, content []byte) bool {
		id := blobID(format, content)
		out, err := repo.Run("log", "--all", "--format=", "--raw", "--no-abbrev", "--", "projects/"+rel)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(out, "\n") {
			// ":100644 100644 <old-blob> <new-blob> M\tpath"
			f := strings.Fields(line)
			if len(f) >= 4 && strings.HasPrefix(f[0], ":") && (f[2] == id || f[3] == id) {
				return true
			}
		}
		return false
	}
}

// blobID computes git's object id for content stored as a blob.
func blobID(format string, content []byte) string {
	var h hash.Hash
	if format == "sha256" {
		h = sha256.New()
	} else {
		h = sha1.New()
	}
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

// MigrationState is the per-PC record of legacy mirror file versions already
// imported into their fingerprint keys. It lives at
// ~/.claudesync/.state/migrated.json (gitignored, like the manifest).
type MigrationState struct {
	imported map[string]string // legacy rel (localHash/memory/name) → content sha256
}

type migrationStateJSON struct {
	Version  int               `json:"version"`
	Imported map[string]string `json:"imported"`
}

const (
	migrationFileName = "migrated.json"
	migrationVersion  = 1
)

// NewMigrationState returns an empty state.
func NewMigrationState() *MigrationState {
	return &MigrationState{imported: map[string]string{}}
}

// MigrationStatePath returns the state file's path inside syncDir.
func MigrationStatePath(syncDir string) string {
	return filepath.Join(syncDir, manifestStateDir, migrationFileName)
}

// LoadMigrationState reads the state; a missing file yields an empty state.
func LoadMigrationState(syncDir string) (*MigrationState, error) {
	b, err := os.ReadFile(MigrationStatePath(syncDir))
	if errors.Is(err, fs.ErrNotExist) {
		return NewMigrationState(), nil
	}
	if err != nil {
		return nil, err
	}
	var raw migrationStateJSON
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	s := NewMigrationState()
	for k, v := range raw.Imported {
		s.imported[k] = v
	}
	return s, nil
}

// Save writes the state to syncDir.
func (s *MigrationState) Save(syncDir string) error {
	b, err := json.MarshalIndent(migrationStateJSON{Version: migrationVersion, Imported: s.imported}, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := os.MkdirAll(filepath.Dir(MigrationStatePath(syncDir)), 0700); err != nil {
		return err
	}
	return os.WriteFile(MigrationStatePath(syncDir), b, 0600)
}

// Imported returns the content hash recorded for a legacy file, or "".
func (s *MigrationState) Imported(legacyRel string) string { return s.imported[legacyRel] }

// Record notes that this content of a legacy file has been imported.
func (s *MigrationState) Record(legacyRel, sum string) { s.imported[legacyRel] = sum }

// Forget drops a legacy file's record (after it is pruned).
func (s *MigrationState) Forget(legacyRel string) { delete(s.imported, legacyRel) }
