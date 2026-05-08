// Package gitwt is a thin wrapper around the system `git` binary, scoped to a
// single working tree (the sync repo). We shell out rather than use go-git
// because we need full git features — specifically custom merge drivers,
// which go-git does not implement.
package gitwt

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
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

// Pull does `git pull --rebase --autostash origin <branch>`.
func (r *Repo) Pull(branch string) error {
	_, err := r.Run("pull", "--rebase", "--autostash", "origin", branch)
	return err
}

// Push does `git push origin <branch>`. The first push from a new local repo
// uses --set-upstream automatically here.
func (r *Repo) Push(branch string) error {
	_, err := r.Run("push", "--set-upstream", "origin", branch)
	return err
}

// CurrentBranch returns the name of the currently checked-out branch.
func (r *Repo) CurrentBranch() (string, error) {
	out, err := r.Run("rev-parse", "--abbrev-ref", "HEAD")
	return strings.TrimSpace(out), err
}

// FetchOriginBranch returns whether the named branch exists on origin.
// Used to decide between "first push" and "pull then push" on init.
func (r *Repo) FetchOriginBranch(branch string) (bool, error) {
	out, err := r.Run("ls-remote", "--heads", "origin", branch)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}
