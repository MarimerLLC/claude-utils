package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	syncpkg "github.com/MarimerLLC/claude-utils/internal/sync"
)

const (
	defaultBranch    = "main"
	mergeDriverName  = "claude-memory-index"
	gitattributesRel = ".gitattributes"
)

// runInit handles `claude-memsync init [flags]`.
func runInit(args []string) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	remote := flags.String("remote", "", "git remote URL to sync against (e.g. git@github.com:user/claude-memories.git)")
	syncDir := flags.String("sync-dir", "", "override sync work-tree (default: ~/.claudesync)")
	claudeDir := flags.String("claude-dir", "", "override Claude projects directory (default: ~/.claude/projects)")
	force := flags.Bool("force", false, "re-init even if sync-dir already exists")

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *remote == "" {
		fmt.Fprintln(os.Stderr, "init: --remote is required")
		flags.Usage()
		return 2
	}

	cfg := config.Defaults()
	if *syncDir != "" {
		cfg.SyncDir = *syncDir
	}
	if *claudeDir != "" {
		cfg.ClaudeProjectsDir = *claudeDir
	}
	cfg.RemoteURL = *remote

	driver, err := config.DiscoverMergeDriver()
	if err != nil {
		fmt.Fprintln(os.Stderr, "init: cannot locate claude-memmerge:", err)
		return 1
	}
	cfg.MergeDriverPath = driver

	if err := bootstrap(cfg, *force); err != nil {
		fmt.Fprintln(os.Stderr, "init failed:", err)
		return 1
	}
	return 0
}

func bootstrap(cfg config.Config, force bool) error {
	// Step 1: ensure SyncDir exists / is empty.
	info, err := os.Stat(cfg.SyncDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// fine, will be created by clone
	case err != nil:
		return fmt.Errorf("stat %s: %w", cfg.SyncDir, err)
	default:
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", cfg.SyncDir)
		}
		if !force {
			entries, _ := os.ReadDir(cfg.SyncDir)
			if len(entries) > 0 {
				return fmt.Errorf("%s already exists and is non-empty (pass --force to re-use)", cfg.SyncDir)
			}
		}
	}

	repo := gitwt.New(cfg.SyncDir)

	// Step 2: clone, falling back to init+remote-add for empty repos.
	if err := os.MkdirAll(filepath.Dir(cfg.SyncDir), 0700); err != nil {
		return err
	}
	cloned, cloneErr := tryClone(repo, cfg.RemoteURL)
	if !cloned {
		// Clone failed (likely empty repo) — init local and add remote.
		fmt.Fprintln(os.Stderr, "clone returned empty/failed, initializing locally:", cloneErr)
		if err := repo.Init(defaultBranch); err != nil {
			return fmt.Errorf("init local repo: %w", err)
		}
		if err := repo.SetRemote(cfg.RemoteURL); err != nil {
			return fmt.Errorf("add remote: %w", err)
		}
	}

	// Step 3: configure merge driver and gitattributes.
	if err := repo.ConfigSet(fmt.Sprintf("merge.%s.name", mergeDriverName), "claude memory index merge"); err != nil {
		return err
	}
	driverCmd := fmt.Sprintf("%s %%O %%A %%B %%L %%P", quoteIfSpaces(cfg.MergeDriverPath))
	if err := repo.ConfigSet(fmt.Sprintf("merge.%s.driver", mergeDriverName), driverCmd); err != nil {
		return err
	}
	if err := writeGitattributes(cfg.SyncDir); err != nil {
		return fmt.Errorf("write .gitattributes: %w", err)
	}

	// Step 4: write config.json.
	if err := config.Save(cfg); err != nil {
		return fmt.Errorf("save config: %w", err)
	}

	// Step 5: bidirectional reconcile of Claude tree ↔ mirror.
	mirrorProjects := filepath.Join(cfg.SyncDir, "projects")
	if err := os.MkdirAll(mirrorProjects, 0700); err != nil {
		return err
	}
	roots := syncpkg.Roots{Claude: cfg.ClaudeProjectsDir, Mirror: mirrorProjects}
	report, err := syncpkg.Reconcile(roots)
	if err != nil {
		return fmt.Errorf("reconcile: %w", err)
	}
	printReport(report)

	// Step 6: stage + commit + push.
	if err := repo.AddAll(); err != nil {
		return fmt.Errorf("git add: %w", err)
	}
	committed, err := repo.Commit("init: bootstrap from " + hostname())
	if err != nil {
		return fmt.Errorf("git commit: %w", err)
	}
	if committed {
		fmt.Fprintln(os.Stderr, "committed bootstrap snapshot")
	} else {
		fmt.Fprintln(os.Stderr, "no local changes to commit")
	}
	if cloned || committed {
		if err := repo.Push(defaultBranch); err != nil {
			return fmt.Errorf("git push: %w", err)
		}
		fmt.Fprintln(os.Stderr, "pushed to", cfg.RemoteURL)
	}

	fmt.Fprintln(os.Stderr, "init OK")
	fmt.Fprintln(os.Stderr, "  sync dir:        ", cfg.SyncDir)
	fmt.Fprintln(os.Stderr, "  claude projects: ", cfg.ClaudeProjectsDir)
	fmt.Fprintln(os.Stderr, "  remote:          ", cfg.RemoteURL)
	fmt.Fprintln(os.Stderr, "  merge driver:    ", cfg.MergeDriverPath)
	return nil
}

// tryClone attempts `git clone <url> <dir>`. Returns (true, nil) on success,
// (false, err) on failure. An empty-repo error is one common failure.
func tryClone(repo *gitwt.Repo, url string) (bool, error) {
	if err := repo.Clone(url); err != nil {
		return false, err
	}
	return true, nil
}

func writeGitattributes(syncDir string) error {
	path := filepath.Join(syncDir, gitattributesRel)
	content := fmt.Sprintf("# Managed by claude-memsync — semantic merge for memory index files.\nMEMORY.md merge=%s\n", mergeDriverName)
	return os.WriteFile(path, []byte(content), 0600)
}

func quoteIfSpaces(p string) string {
	if strings.ContainsAny(p, " \t") {
		return `"` + p + `"`
	}
	return p
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}

func printReport(r syncpkg.SyncReport) {
	if len(r.CopiedToMirror) > 0 {
		fmt.Fprintf(os.Stderr, "copied %d files Claude → mirror\n", len(r.CopiedToMirror))
	}
	if len(r.CopiedToClaude) > 0 {
		fmt.Fprintf(os.Stderr, "copied %d files mirror → Claude\n", len(r.CopiedToClaude))
	}
	if len(r.Merged) > 0 {
		fmt.Fprintf(os.Stderr, "semantically merged %d MEMORY.md files\n", len(r.Merged))
	}
	if len(r.BackedUp) > 0 {
		fmt.Fprintf(os.Stderr, "%d collisions saved as .from-remote-* in the mirror\n", len(r.BackedUp))
	}
}
