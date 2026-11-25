// Package gitwt is a thin wrapper around the system `git` binary, scoped to a
// single working tree (the sync repo). We shell out rather than use go-git
// because we need full git features — specifically custom merge drivers,
// which go-git does not implement.
package gitwt

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/MarimerLLC/claude-utils/internal/proc"
)

// Repo represents a local git working tree at a fixed path.
type Repo struct {
	Dir string
}

// New constructs a Repo for the given working directory. The directory does
// not need to exist yet; pass it through Init or Clone first.
func New(dir string) *Repo { return &Repo{Dir: dir} }

// Run executes `git -C <dir> <args...>` and returns stdout. Stderr is
// included in the returned error on non-zero exit.
func (r *Repo) Run(args ...string) (string, error) {
	full := append([]string{"-C", r.Dir}, args...)
	cmd := exec.Command("git", full...)
	proc.HideWindow(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// IsRepo reports whether r.Dir is inside an existing git work-tree.
func (r *Repo) IsRepo() bool {
	_, err := r.Run("rev-parse", "--is-inside-work-tree")
	return err == nil
}

// Clone clones url into r.Dir. r.Dir must not already exist or must be empty.
func (r *Repo) Clone(url string) error {
	cmd := exec.Command("git", "clone", url, r.Dir)
	proc.HideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git clone %s: %w: %s", url, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Init runs `git init` in r.Dir. The directory is created if missing.
func (r *Repo) Init(defaultBranch string) error {
	cmd := exec.Command("git", "init", "-b", defaultBranch, r.Dir)
	proc.HideWindow(cmd)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// SetRemote sets origin to url, replacing any existing origin.
func (r *Repo) SetRemote(url string) error {
	if _, err := r.Run("remote", "remove", "origin"); err != nil {
		// ignore: origin may not exist
	}
	_, err := r.Run("remote", "add", "origin", url)
	return err
}

// ConfigSet writes a key=value pair to the local repo config.
func (r *Repo) ConfigSet(key, value string) error {
	_, err := r.Run("config", key, value)
	return err
}

// AddAll stages all changes (including deletions) under the work-tree.
func (r *Repo) AddAll() error {
	_, err := r.Run("add", "-A")
	return err
}

// HasStagedChanges reports whether there is anything staged that would be
// included in a commit.
func (r *Repo) HasStagedChanges() (bool, error) {
	_, err := r.Run("diff", "--cached", "--quiet")
	if err == nil {
		return false, nil // exit 0 = no diff
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return true, nil // exit 1 = has diff (per git diff --quiet contract)
	}
	return false, err
}

// Commit creates a commit if anything is staged. Returns true if a commit was made.
func (r *Repo) Commit(message string) (bool, error) {
	has, err := r.HasStagedChanges()
	if err != nil {
		return false, err
	}
	if !has {
		return false, nil
	}
	_, err = r.Run("commit", "-m", message)
	return err == nil, err
}

// Pull does `git pull --no-rebase --ff --no-edit --autostash origin <branch>`.
//
// It always merges rather than rebasing: a rebase replays every local commit
// and can stop on the same conflict once per commit, while a merge meets each
// conflict exactly once. --ff (fast-forward when possible, merge otherwise)
// overrides a user's global pull.ff=only, which would refuse to merge. A
// merge that stops on a conflict leaves the work-tree mid-merge (see
// MergeInProgress) for the caller to resolve or abort.
func (r *Repo) Pull(branch string) error {
	_, err := r.Run("pull", "--no-rebase", "--ff", "--no-edit", "--autostash", "origin", branch)
	return err
}

// gitPathExists reports whether the repo-internal path name (as resolved by
// `git rev-parse --git-path`) exists.
func (r *Repo) gitPathExists(name string) bool {
	out, err := r.Run("rev-parse", "--git-path", name)
	if err != nil {
		return false
	}
	p := strings.TrimSpace(out)
	if p == "" {
		return false
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.Dir, p)
	}
	_, err = os.Stat(p)
	return err == nil
}

// RebaseInProgress reports whether the work-tree is stopped mid-rebase. Pull
// no longer rebases, but an older daemon may have died mid-rebase.
func (r *Repo) RebaseInProgress() bool {
	return r.gitPathExists("rebase-merge") || r.gitPathExists("rebase-apply")
}

// AbortRebase runs `git rebase --abort`, restoring the pre-pull state (and any
// autostashed changes) so later syncs aren't stuck behind a half-done rebase.
func (r *Repo) AbortRebase() error {
	_, err := r.Run("rebase", "--abort")
	return err
}

// MergeInProgress reports whether the work-tree is stopped mid-merge (a pull
// whose merge hit a conflict).
func (r *Repo) MergeInProgress() bool {
	return r.gitPathExists("MERGE_HEAD")
}

// AbortMerge runs `git merge --abort`, restoring the pre-pull state (and any
// autostashed changes).
func (r *Repo) AbortMerge() error {
	_, err := r.Run("merge", "--abort")
	return err
}

// CommitMerge concludes a merge whose conflicts have all been resolved and
// staged.
func (r *Repo) CommitMerge(message string) error {
	_, err := r.Run("commit", "--no-edit", "-m", message)
	return err
}

// Unmerged is one conflicted path in the index, with the stages present:
// stage 1 is the common ancestor (base), 2 is ours (HEAD), 3 is theirs.
type Unmerged struct {
	Path      string // repo-relative, slash-separated, exactly as git stores it
	HasBase   bool
	HasOurs   bool
	HasTheirs bool
}

// UnmergedEntries lists the conflicted paths in the index, in index order.
func (r *Repo) UnmergedEntries() ([]Unmerged, error) {
	out, err := r.Run("ls-files", "-u", "-z")
	if err != nil {
		return nil, err
	}
	return parseUnmerged(out)
}

// parseUnmerged parses `git ls-files -u -z` output: NUL-terminated records of
// "<mode> <object> <stage>\t<path>". The path is taken verbatim, so names
// with spaces, tabs or newlines are safe.
func parseUnmerged(out string) ([]Unmerged, error) {
	var entries []Unmerged
	index := map[string]int{}
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "" {
			continue
		}
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			return nil, fmt.Errorf("ls-files -u: malformed record %q", rec)
		}
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return nil, fmt.Errorf("ls-files -u: malformed record %q", rec)
		}
		i, seen := index[path]
		if !seen {
			i = len(entries)
			index[path] = i
			entries = append(entries, Unmerged{Path: path})
		}
		switch fields[2] {
		case "1":
			entries[i].HasBase = true
		case "2":
			entries[i].HasOurs = true
		case "3":
			entries[i].HasTheirs = true
		default:
			return nil, fmt.Errorf("ls-files -u: unexpected stage %q for %q", fields[2], path)
		}
	}
	return entries, nil
}

// ShowStage returns the content of path at index stage n (1 base, 2 ours,
// 3 theirs).
func (r *Repo) ShowStage(n int, path string) ([]byte, error) {
	out, err := r.Run("show", fmt.Sprintf(":%d:%s", n, path))
	if err != nil {
		return nil, err
	}
	return []byte(out), nil
}

// Push does `git push origin <branch>`. The first push from a new local repo
// uses --set-upstream automatically here.
func (r *Repo) Push(branch string) error {
	_, err := r.Run("push", "--set-upstream", "origin", branch)
	return err
}

// AheadBehind counts the commits HEAD has that origin/<branch> lacks (ahead)
// and the reverse (behind). It reads only the local remote-tracking ref, so
// it makes no network call and reflects origin as of the last fetch.
func (r *Repo) AheadBehind(branch string) (ahead, behind int, err error) {
	out, err := r.Run("rev-list", "--left-right", "--count", "HEAD...refs/remotes/origin/"+branch)
	if err != nil {
		return 0, 0, err
	}
	if _, err := fmt.Sscanf(strings.TrimSpace(out), "%d %d", &ahead, &behind); err != nil {
		return 0, 0, fmt.Errorf("rev-list --count: unexpected output %q", out)
	}
	return ahead, behind, nil
}

// CurrentBranch returns the name of the currently checked-out branch.
func (r *Repo) CurrentBranch() (string, error) {
	out, err := r.Run("rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

// FetchOriginBranch returns whether the named branch exists on origin.
// Used to decide between "first push" and "pull then push" on init.
func (r *Repo) FetchOriginBranch(branch string) (bool, error) {
	sha, err := r.LsRemoteHead(branch)
	if err != nil {
		return false, err
	}
	return sha != "", nil
}

// LsRemoteHead asks origin for the current SHA of refs/heads/<branch>.
// Returns ("", nil) if origin has no such branch yet. This is a single
// network round-trip with no working-tree side effects — used as a cheap
// gate to skip full pull/push cycles when the remote hasn't moved.
func (r *Repo) LsRemoteHead(branch string) (string, error) {
	out, err := r.Run("ls-remote", "--heads", "origin", branch)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(out)
	if line == "" {
		return "", nil
	}
	// Format: "<sha>\trefs/heads/<branch>"
	if i := strings.IndexAny(line, " \t"); i > 0 {
		return line[:i], nil
	}
	return "", nil
}
