package sync

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/project"
)

// RedirectFileName is the file pruneLegacyDirs leaves in a legacy mirror dir
// (projects/<localHash>/moved-to) naming the fingerprint key its memories now
// live under. It sits outside memory/, so it is never synced into Claude's tree,
// and older versions ignore it.
//
// It exists for the other PCs. A PC that keys the same project folder by path
// (no transcript or folder on disk lets it fingerprint the project) used to read
// the pruned files as "the user deleted these memories" and remove its local
// copies — and had it kept them instead, its next reconcile would write them
// straight back into the legacy dir for the pruning PC to import and prune
// again. Following the redirect moves it onto the key with everyone else.
const RedirectFileName = "moved-to"

// writeRedirect records key in legacyProj/moved-to unless a redirect is already
// there. The first PC to prune wins: a PC that fingerprints the same path
// differently (a clone with no origin remote, say) must not flip a redirect
// other PCs already follow.
func writeRedirect(legacyProj, key string) error {
	if hasRedirect(legacyProj) {
		return nil
	}
	return writeFile(filepath.Join(legacyProj, RedirectFileName), []byte(key+"\n"))
}

func hasRedirect(legacyProj string) bool {
	_, err := os.Stat(filepath.Join(legacyProj, RedirectFileName))
	return err == nil
}

// readRedirect returns the fingerprint key a legacy mirror dir redirects to.
func readRedirect(mirror, legacyKey string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(mirror, legacyKey, RedirectFileName))
	if err != nil {
		return "", false
	}
	key := strings.TrimSpace(string(b))
	if !project.IsFingerprintKey(key) {
		return "", false // hand-edited or conflicted: ignore rather than guess
	}
	return key, true
}

// FollowRedirects re-keys every path-keyed project whose legacy mirror dir
// carries a redirect, and returns the local project dirs it moved. Call it after
// each index build and after each pull, before inbound changes are applied, so a
// pruned legacy dir's deletions target no local project. Follow it with
// AdoptRekeyed, which brings the moved projects' local copies onto the key.
func FollowRedirects(r Roots, idx *project.Index) []string {
	all := idx.All()
	locals := make([]string, 0, len(all))
	for lh, e := range all {
		if e.Method == project.MethodPath && e.Key == lh {
			locals = append(locals, lh)
		}
	}
	sort.Strings(locals)

	var moved []string
	for _, localHash := range locals {
		key, ok := readRedirect(r.Mirror, localHash)
		if !ok {
			continue
		}
		e := all[localHash]
		idx.Set(localHash, project.Entry{Key: key, Method: project.MethodMoved, Path: e.Path, Stamp: e.Stamp})
		moved = append(moved, localHash)
	}
	return moved
}

// AdoptRekeyed handles every local project whose key differs between before
// and idx: one decoded from its folder name, one following a redirect, a clone
// that gained an origin remote. Its local memory files are copies last synced
// under the old key, and the new key may hold newer versions merged by other
// PCs. Left alone, the next reconcile would treat each differing local copy as
// this PC's edit and let it overwrite the key's newer one.
//
// So each local file is first folded into the key exactly as a legacy dir is
// imported (importVersion: a version the key already moved past gives way, an
// absent one is copied in, real edits are merged against the history the two
// keys share), and the result is written back to the local copy. A file the key
// held and has since deleted is removed locally. Reconcile then only has to
// bring down the key's other files and send up anything new.
//
// A project whose adoption fails is put back on its old key (so reconcile
// can't clobber the new one) and retried on the next index build. It returns
// the local project dirs it adopted.
func AdoptRekeyed(r Roots, before map[string]project.Entry, idx *project.Index, opt MigrateOptions) ([]string, SyncReport) {
	var rep SyncReport
	after := idx.All()
	locals := make([]string, 0, len(after))
	for lh := range after {
		locals = append(locals, lh)
	}
	sort.Strings(locals)

	var adopted []string
	for _, localHash := range locals {
		old, had := before[localHash]
		cur := after[localHash]
		if !had || old.Key == cur.Key {
			continue
		}
		if err := adoptLocalCopies(r, localHash, old.Key, cur.Key, opt, &rep); err != nil {
			idx.Set(localHash, old)
			rep.Errors = append(rep.Errors, fmt.Sprintf("re-key %s %s -> %s: %v (kept old key)", localHash, old.Key, cur.Key, err))
			continue
		}
		adopted = append(adopted, localHash)
	}
	return adopted, rep
}

func adoptLocalCopies(r Roots, localHash, oldKey, key string, opt MigrateOptions, rep *SyncReport) error {
	localMem := filepath.Join(r.Claude, localHash, "memory")
	keyMem := filepath.Join(r.Mirror, key, "memory")
	entries, err := os.ReadDir(localMem)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || isCollisionBackup(name) {
			continue
		}
		lp := filepath.Join(localMem, name)
		local, err := os.ReadFile(lp)
		if err != nil {
			return err
		}
		oldRel := filepath.ToSlash(filepath.Join(oldKey, "memory", name))
		rel := filepath.ToSlash(filepath.Join(key, "memory", name))
		kp := filepath.Join(keyMem, name)
		if err := importVersion(kp, local, oldRel, rel, opt, rep); err != nil {
			return err
		}
		switch exists, keyCopy := readIfExists(kp); {
		case !exists:
			// Only a version the key held and then deleted is skipped by the
			// import; follow the deletion.
			if err := os.Remove(lp); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
			rep.RemovedFromClaude = append(rep.RemovedFromClaude, filepath.ToSlash(filepath.Join(localHash, "memory", name)))
		case !bytes.Equal(local, keyCopy):
			if err := writeFile(lp, keyCopy); err != nil {
				return err
			}
			rep.CopiedToClaude = append(rep.CopiedToClaude, filepath.ToSlash(filepath.Join(localHash, "memory", name)))
		}
	}
	return nil
}
