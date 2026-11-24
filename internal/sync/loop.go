package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/distill"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/project"
)

// Loop is the long-running daemon body. It watches the Claude memory tree,
// mirrors file changes into the sync work-tree, and runs git commit/pull/push
// cycles on a debounce.
type Loop struct {
	Cfg      config.Config
	Branch   string
	Hostname string
	OnFlush  func(localChanges bool, err error) // optional, for tests/observability

	// idx maps localHash → fingerprint key for the current Claude tree. Seeded
	// from the saved index at startup and rebuilt then, on the pull tick, and
	// whenever an event needs a full reconcile (e.g. a project opened
	// mid-session). The loop is single-goroutine, so no locking is needed.
	idx *project.Index

	// migState records legacy mirror files already imported (see MigrateLayout).
	migState *MigrationState

	// needReconcile is set when a watcher event couldn't be applied
	// incrementally (ErrNeedsReconcile); the next debounced flush reconciles
	// before committing.
	needReconcile bool
}

// Run blocks until ctx is canceled. Returns the first fatal error or nil
// when ctx ends. Recoverable errors (transient git failures) are logged.
func (l *Loop) Run(ctx context.Context) error {
	if l.Branch == "" {
		l.Branch = "main"
	}
	if l.Hostname == "" {
		h, _ := os.Hostname()
		l.Hostname = h
	}

	repo := gitwt.New(l.Cfg.SyncDir)
	if !repo.IsRepo() {
		return fmt.Errorf("sync dir %s is not a git repo — run `claude-memsync init` first", l.Cfg.SyncDir)
	}

	mirrorProjects := filepath.Join(l.Cfg.SyncDir, "projects")
	roots := Roots{Claude: l.Cfg.ClaudeProjectsDir, Mirror: mirrorProjects}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create watcher: %w", err)
	}
	defer watcher.Close()

	if err := os.MkdirAll(roots.Claude, 0700); err != nil {
		return err
	}
	if err := watcher.Add(roots.Claude); err != nil {
		return fmt.Errorf("watch %s: %w", roots.Claude, err)
	}
	if err := refreshMemoryWatches(watcher, roots.Claude); err != nil {
		log.Println("refresh memory watches:", err)
	}

	// A foreground `run` is otherwise silent, which reads as a hang; announce
	// startup so it's clear the daemon is alive and what it's doing.
	log.Printf("watching %s → %s (branch %s); pull every %ds. this runs in the foreground — Ctrl-C to stop.",
		roots.Claude, l.Cfg.RemoteURL, l.Branch, l.Cfg.PullIntervalSec)

	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	debounceDur := time.Duration(l.Cfg.DebounceMs) * time.Millisecond
	pullTicker := time.NewTicker(time.Duration(l.Cfg.PullIntervalSec) * time.Second)
	defer pullTicker.Stop()

	pendingLocal := false

	// Build the project index (seeded from the saved one, so keys stay stable
	// for projects whose checkout has since gone away) and import any legacy
	// path-keyed mirror dirs into fingerprint keys before the first sync, so
	// pre-existing per-machine dirs collapse to one key.
	if saved, err := project.Load(l.Cfg.SyncDir); err != nil {
		log.Println("load project index (rebuilding from scratch):", err)
	} else {
		l.idx = saved
	}
	if st, err := LoadMigrationState(l.Cfg.SyncDir); err != nil {
		log.Println("load migration state (re-importing legacy dirs):", err)
		l.migState = NewMigrationState()
	} else {
		l.migState = st
	}
	l.rebuildIndex(roots)
	l.migrate(repo, roots)

	// Initial sync at startup: reconcile local with mirror (using manifest
	// to detect deletes that happened while the daemon was off), then flush.
	manifest, err := LoadManifest(l.Cfg.SyncDir)
	if err != nil {
		log.Println("load manifest (continuing without delete detection):", err)
		manifest = NewManifest()
	}
	if _, err := Reconcile(roots, l.idx, manifest); err != nil {
		log.Println("initial reconcile:", err)
	}
	if err := l.flush(repo, roots, true); err != nil {
		log.Println("initial flush:", err)
		if l.OnFlush != nil {
			l.OnFlush(true, err)
		}
	} else if l.OnFlush != nil {
		l.OnFlush(true, nil)
	}
	l.refreshManifest(roots)
	l.rebuildDistilledIndex()
	log.Printf("initial sync complete; tracking %d project(s). idle until a memory changes.", len(l.idx.All()))

	for {
		select {
		case <-ctx.Done():
			return nil

		case ev, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			handled, err := l.handleEvent(watcher, roots, ev)
			if err != nil {
				log.Println("event:", err)
				continue
			}
			if !handled {
				continue
			}
			pendingLocal = true
			debounce.Reset(debounceDur)

		case err, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			log.Println("watcher error:", err)

		case <-debounce.C:
			if l.needReconcile {
				// An event couldn't be applied incrementally (new project, or a
				// sibling checkout with its own edit); re-index and reconcile so
				// the change merges instead of overwriting, without waiting for
				// the pull tick.
				l.needReconcile = false
				l.rebuildIndex(roots)
				m, err := LoadManifest(l.Cfg.SyncDir)
				if err != nil {
					log.Println("load manifest:", err)
					m = NewManifest()
				}
				if _, err := Reconcile(roots, l.idx, m); err != nil {
					log.Println("reconcile:", err)
				}
			}
			err := l.flush(repo, roots, true)
			if err != nil {
				log.Println("flush (local):", err)
			} else {
				log.Println("synced local memory changes")
			}
			l.refreshManifest(roots)
			l.rebuildDistilledIndex()
			if l.OnFlush != nil {
				l.OnFlush(true, err)
			}
			pendingLocal = false

		case <-pullTicker.C:
			if pendingLocal {
				continue // debounce will handle it
			}
			// Safety net: re-walk the Claude tree to catch any watcher events
			// missed by races (e.g., a new project dir and memory file created
			// in rapid succession before the new dir's watcher was registered).
			// Using the on-disk manifest also lets us propagate deletes the
			// watcher missed.
			m, err := LoadManifest(l.Cfg.SyncDir)
			if err != nil {
				log.Println("load manifest:", err)
				m = NewManifest()
			}
			// Refresh the index so projects opened since startup (and any repo
			// whose remote changed) resolve to the right key, and import edits
			// pre-upgrade PCs made to legacy dirs, before reconciling.
			l.rebuildIndex(roots)
			l.migrate(repo, roots)
			if _, err := Reconcile(roots, l.idx, m); err != nil {
				log.Println("reconcile:", err)
			}
			err = l.flush(repo, roots, true)
			if err != nil {
				log.Println("flush (pull):", err)
			}
			l.refreshManifest(roots)
			l.rebuildDistilledIndex()
			if l.OnFlush != nil {
				l.OnFlush(false, err)
			}
		}
	}
}

// handleEvent filters and reacts to a single fsnotify event. Returns
// (handled, error). handled=true means the change was relevant and should
// trigger a debounced flush.
func (l *Loop) handleEvent(watcher *fsnotify.Watcher, roots Roots, ev fsnotify.Event) (bool, error) {
	rel, err := filepath.Rel(roots.Claude, ev.Name)
	if err != nil {
		return false, nil
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")

	// Discover newly-created project dirs and start watching their memory subdir.
	if ev.Op&fsnotify.Create != 0 {
		if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
			// either a new <hash>/ dir or a new memory/ inside one we already watched.
			_ = watcher.Add(ev.Name) // best-effort
			_ = refreshMemoryWatches(watcher, roots.Claude)
			return false, nil
		}
	}

	// We only care about files under <hash>/memory/<filename>.
	if len(parts) != 3 || parts[1] != "memory" {
		return false, nil
	}
	hash, name := parts[0], parts[2]
	if shouldIgnoreFile(name) {
		return false, nil
	}

	var opErr error
	var op string
	switch {
	case ev.Op&(fsnotify.Create|fsnotify.Write) != 0:
		op = "copy to mirror"
		opErr = CopyToMirror(roots, l.idx, hash, name)
		if errors.Is(opErr, fs.ErrNotExist) {
			// Race: file vanished between event and read. Treat as remove.
			op = "remove from mirror"
			opErr = RemoveFromMirror(roots, l.idx, hash, name)
		}
	case ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
		op = "remove from mirror"
		opErr = RemoveFromMirror(roots, l.idx, hash, name)
	default:
		return false, nil
	}
	if errors.Is(opErr, ErrNeedsReconcile) {
		// A new/unkeyed project, or a sibling checkout with its own edit: don't
		// copy blindly; reconcile (merge / back up) on the debounced flush.
		l.needReconcile = true
		return true, nil
	}
	if opErr != nil {
		return false, fmt.Errorf("%s %s/%s: %w", op, hash, name, opErr)
	}
	return true, nil
}

// flush stages local changes, optionally commits, pulls remote work, propagates
// any pull-driven changes back to the Claude tree, and pushes.
//
// Before doing any pull/push, flush asks origin for the current branch SHA
// (a single ls-remote round-trip). If origin's SHA matches our cached
// refs/remotes/origin/<branch> AND we have no unpushed commits, it returns
// early — skipping the full pull/push churn that dominates idle ticks.
func (l *Loop) flush(repo *gitwt.Repo, roots Roots, includeLocal bool) error {
	if includeLocal {
		if err := repo.AddAll(); err != nil {
			return fmt.Errorf("add: %w", err)
		}
		_, err := repo.Commit(fmt.Sprintf("auto: %s @ %s", l.Hostname, time.Now().UTC().Format(time.RFC3339)))
		if err != nil {
			return fmt.Errorf("commit: %w", err)
		}
	}

	if idle, err := remoteAndLocalIdle(repo, l.Branch); err == nil && idle {
		return nil
	}

	preRev, _ := repo.Run("rev-parse", "HEAD")
	if err := repo.Pull(l.Branch); err != nil {
		// Don't propagate first-time failure (no upstream yet); push will create it.
		log.Println("pull:", err)
		// A conflict the merge driver can't resolve (modify/delete, a non-
		// MEMORY.md overlap) leaves the work-tree mid-rebase, which would wedge
		// every later sync. Back out; the local commits stay and are retried.
		if repo.RebaseInProgress() {
			if aerr := repo.AbortRebase(); aerr != nil {
				log.Println("abort stuck rebase:", aerr)
			} else {
				log.Println("aborted conflicting rebase; will retry on the next sync")
			}
		}
	}
	postRev, _ := repo.Run("rev-parse", "HEAD")
	if strings.TrimSpace(preRev) != strings.TrimSpace(postRev) && strings.TrimSpace(preRev) != "" {
		if err := propagateChanges(repo, roots, l.idx, strings.TrimSpace(preRev), strings.TrimSpace(postRev)); err != nil {
			return fmt.Errorf("propagate: %w", err)
		}
	}

	if err := repo.Push(l.Branch); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// remoteAndLocalIdle reports whether there is nothing to do: origin's branch
// SHA, our cached remote-tracking ref, and HEAD all point at the same commit.
//
// Comparing HEAD to the tracking ref (rather than only counting unpushed
// commits) also catches the case where the tracking ref was advanced by a
// fetch that HEAD never caught up with — a manual `git fetch` in the sync
// dir, or a pull whose fetch succeeded but whose rebase failed. Without that
// check the gate sees "remote unchanged, nothing unpushed" and the PC stays
// behind until some other PC pushes again.
//
// Returns (false, nil) when the gate cannot prove idleness — including the
// first run before refs/remotes/origin/<branch> exists locally.
func remoteAndLocalIdle(repo *gitwt.Repo, branch string) (bool, error) {
	remoteSHA, err := repo.LsRemoteHead(branch)
	if err != nil || remoteSHA == "" {
		return false, err
	}
	cachedSHA, err := repo.Run("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+branch)
	if err != nil {
		return false, nil // remote-tracking ref missing → can't prove idle
	}
	if strings.TrimSpace(cachedSHA) != remoteSHA {
		return false, nil // remote moved
	}
	headSHA, err := repo.Run("rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		return false, nil
	}
	// HEAD ahead (unpushed), behind (fetched but not merged), or diverged
	// all mean there's work for pull/push to do.
	return strings.TrimSpace(headSHA) == remoteSHA, nil
}

// propagateChanges replays the file changes between pre and post revisions from
// the mirror back into Claude's tree. Mirror paths are keyed by fingerprint, so
// each key is reverse-resolved via idx to the local project dir(s) it belongs to;
// a key with no local project is skipped (it materializes on next reconcile once
// that project is opened here).
func propagateChanges(repo *gitwt.Repo, roots Roots, idx *project.Index, pre, post string) error {
	out, err := repo.Run("diff", "--name-status", pre+".."+post)
	if err != nil {
		return err
	}
	applied := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		status := fields[0]
		// For renames the format is "R<score> oldpath newpath". Treat newpath
		// as the canonical post-image; mark old as removed.
		var path string
		if strings.HasPrefix(status, "R") && len(fields) >= 3 {
			oldPath := fields[1]
			path = fields[2]
			if key, name, ok := splitMirrorPath(oldPath); ok {
				applied += removeFromClaude(roots, idx, key, name)
			}
		} else {
			path = fields[1]
		}
		key, name, ok := splitMirrorPath(path)
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(status, "D"):
			applied += removeFromClaude(roots, idx, key, name)
		default:
			// A key with no local project (opened only on another PC, or a
			// legacy dir MigrateLayout imports) targets nothing and isn't counted.
			if n, err := CopyToClaude(roots, idx, key, name); err != nil {
				log.Printf("propagate %s: %v", path, err)
			} else {
				applied += n
			}
		}
	}
	if applied > 0 {
		log.Printf("applied %d inbound memory change(s) from remote", applied)
	}
	return nil
}

// removeFromClaude deletes a memory file from every local project mapping to
// key, returning how many local projects it targeted.
func removeFromClaude(roots Roots, idx *project.Index, key, name string) int {
	locals := idx.LocalsFor(key)
	for _, localHash := range locals {
		_ = os.Remove(filepath.Join(roots.Claude, localHash, "memory", name))
	}
	return len(locals)
}

// splitMirrorPath converts a path of the form "projects/<key>/memory/<file>"
// (as it appears in `git diff --name-only`) into its parts.
func splitMirrorPath(p string) (key, name string, ok bool) {
	parts := strings.Split(filepath.ToSlash(p), "/")
	if len(parts) != 4 || parts[0] != "projects" || parts[2] != "memory" {
		return "", "", false
	}
	return parts[1], parts[3], true
}

// refreshMemoryWatches walks the Claude tree and ensures each existing
// <hash>/memory/ directory is watched. Idempotent.
func refreshMemoryWatches(w *fsnotify.Watcher, claudeRoot string) error {
	entries, err := os.ReadDir(claudeRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mem := filepath.Join(claudeRoot, e.Name(), "memory")
		if info, err := os.Stat(mem); err == nil && info.IsDir() {
			_ = w.Add(mem) // no-op if already watching
		}
	}
	return nil
}

// migrate imports legacy path-keyed mirror dirs into fingerprint keys (pruning
// them only when configured) and saves the import record. Best-effort.
func (l *Loop) migrate(repo *gitwt.Repo, roots Roots) {
	rep, err := MigrateLayout(roots, l.idx, MigrateOptions{
		State: l.migState,
		Prior: PriorVersionIn(repo),
		Prune: l.Cfg.PruneLegacyDirs,
	})
	if err != nil {
		log.Println("migrate layout:", err)
	}
	if n := len(rep.CopiedToMirror) + len(rep.Merged) + len(rep.BackedUp); n > 0 {
		log.Printf("imported legacy mirror dirs into fingerprint keys (%d files)", n)
	}
	if n := len(rep.PrunedLegacy); n > 0 {
		log.Printf("pruned %d legacy mirror file(s)", n)
	}
	if err := l.migState.Save(l.Cfg.SyncDir); err != nil {
		log.Println("save migration state:", err)
	}
}

// rebuildIndex refreshes the localHash → fingerprint-key mapping from the
// current Claude tree and persists it. The previous index seeds the build so
// unchanged projects skip the git calls and vanished checkouts keep their key.
// Best-effort: on error it keeps any existing index (or an empty one) so the
// loop can proceed.
func (l *Loop) rebuildIndex(roots Roots) {
	idx, err := project.Build(roots.Claude, l.idx)
	if err != nil {
		log.Println("build project index:", err)
		if l.idx == nil {
			l.idx = project.NewIndex()
		}
		return
	}
	l.idx = idx
	if err := idx.Save(l.Cfg.SyncDir); err != nil {
		log.Println("save project index:", err)
	}
}

// refreshManifest rebuilds the manifest from the current Claude tree state
// and saves it. Called after every successful flush so that the next
// Reconcile reasons against an up-to-date "as of last sync" snapshot.
func (l *Loop) refreshManifest(roots Roots) {
	m, err := BuildFromClaudeTree(roots.Claude)
	if err != nil {
		log.Println("build manifest:", err)
		return
	}
	if err := m.Save(l.Cfg.SyncDir); err != nil {
		log.Println("save manifest:", err)
	}
}

// rebuildDistilledIndex regenerates the local DISTILLED.md catalog index from
// the synced entry files. The index is a derived, git-ignored artifact, so this
// never affects the sync repo; it just keeps the local index fresh after entries
// arrive (from the /distill skill locally or via a pull). Best-effort: a missing
// catalog dir is skipped and errors are logged, never fatal.
func (l *Loop) rebuildDistilledIndex() {
	dir := l.Cfg.DistilledPath()
	if _, err := os.Stat(dir); err != nil {
		return // no catalog on this PC yet
	}
	if _, err := distill.BuildIndex(distill.Options{
		ProjectsDir:  l.Cfg.ClaudeProjectsDir,
		DistilledDir: dir,
	}); err != nil {
		log.Println("rebuild distilled index:", err)
	}
}

// shouldIgnoreFile returns true for filenames the daemon should not sync.
func shouldIgnoreFile(name string) bool {
	if name == "" {
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	// Atomic-write temp files: a trailing ".tmp", or the
	// "<name>.tmp.<pid>.<hash>" form left behind by interrupted memory writes
	// (the pid-suffixed variant does not end in ".tmp", so the suffix check
	// alone misses it). Neither is a real memory; they must never sync.
	if strings.HasSuffix(name, ".tmp") || strings.Contains(name, ".tmp.") {
		return true
	}
	return false
}
