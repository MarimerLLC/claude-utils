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
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// Loop is the long-running daemon body. It watches the Claude memory tree,
// mirrors file changes into the sync work-tree, and runs git commit/pull/push
// cycles on a debounce.
type Loop struct {
	Cfg       config.Config
	Branch    string
	Hostname  string
	OnFlush   func(localChanges bool, err error) // optional, for tests/observability
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

	debounce := time.NewTimer(time.Hour)
	if !debounce.Stop() {
		<-debounce.C
	}
	debounceDur := time.Duration(l.Cfg.DebounceMs) * time.Millisecond
	pullTicker := time.NewTicker(time.Duration(l.Cfg.PullIntervalSec) * time.Second)
	defer pullTicker.Stop()

	pendingLocal := false

	// Initial sync at startup: reconcile local with mirror (using manifest
	// to detect deletes that happened while the daemon was off), then flush.
	manifest, err := LoadManifest(l.Cfg.SyncDir)
	if err != nil {
		log.Println("load manifest (continuing without delete detection):", err)
		manifest = NewManifest()
	}
	if _, err := Reconcile(roots, manifest); err != nil {
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
			err := l.flush(repo, roots, true)
			if err != nil {
				log.Println("flush (local):", err)
			}
			l.refreshManifest(roots)
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
			if _, err := Reconcile(roots, m); err != nil {
				log.Println("reconcile:", err)
			}
			err = l.flush(repo, roots, true)
			if err != nil {
				log.Println("flush (pull):", err)
			}
			l.refreshManifest(roots)
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

	switch {
	case ev.Op&(fsnotify.Create|fsnotify.Write) != 0:
		if err := CopyToMirror(roots, hash, name); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				// Race: file vanished between event and read. Treat as remove.
				return true, RemoveFromMirror(roots, hash, name)
			}
			return false, fmt.Errorf("copy to mirror %s/%s: %w", hash, name, err)
		}
		return true, nil

	case ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0:
		if err := RemoveFromMirror(roots, hash, name); err != nil {
			return false, fmt.Errorf("remove from mirror %s/%s: %w", hash, name, err)
		}
		return true, nil
	}
	return false, nil
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
	}
	postRev, _ := repo.Run("rev-parse", "HEAD")
	if strings.TrimSpace(preRev) != strings.TrimSpace(postRev) && strings.TrimSpace(preRev) != "" {
		if err := propagateChanges(repo, roots, strings.TrimSpace(preRev), strings.TrimSpace(postRev)); err != nil {
			return fmt.Errorf("propagate: %w", err)
		}
	}

	if err := repo.Push(l.Branch); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

// remoteAndLocalIdle reports whether there is nothing to do: origin's branch
// SHA matches our cached remote-tracking ref AND we have no unpushed commits.
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
	unpushed, err := repo.Run("rev-list", "--count", "refs/remotes/origin/"+branch+"..HEAD")
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(unpushed) == "0", nil
}

// propagateChanges replays the file changes between pre and post revisions
// from the mirror back into Claude's tree.
func propagateChanges(repo *gitwt.Repo, roots Roots, pre, post string) error {
	out, err := repo.Run("diff", "--name-status", pre+".."+post)
	if err != nil {
		return err
	}
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
			if hash, name, ok := splitMirrorPath(oldPath); ok {
				_ = os.Remove(filepath.Join(roots.Claude, hash, "memory", name))
			}
		} else {
			path = fields[1]
		}
		hash, name, ok := splitMirrorPath(path)
		if !ok {
			continue
		}
		switch {
		case strings.HasPrefix(status, "D"):
			_ = os.Remove(filepath.Join(roots.Claude, hash, "memory", name))
		default:
			if err := CopyToClaude(roots, hash, name); err != nil {
				log.Printf("propagate %s: %v", path, err)
			}
		}
	}
	return nil
}

// splitMirrorPath converts a path of the form "projects/<hash>/memory/<file>"
// (as it appears in `git diff --name-only`) into its parts.
func splitMirrorPath(p string) (hash, name string, ok bool) {
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

// shouldIgnoreFile returns true for filenames the daemon should not sync.
func shouldIgnoreFile(name string) bool {
	if name == "" {
		return true
	}
	if strings.HasPrefix(name, ".") {
		return true
	}
	if strings.HasSuffix(name, ".tmp") {
		return true
	}
	return false
}
