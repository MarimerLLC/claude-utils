package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

// idleFixture creates a bare "origin" plus two clones (a and b) that share
// one pushed commit on main, so both start fully in sync.
func idleFixture(t *testing.T) (a, b *gitwt.Repo) {
	t.Helper()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	mustGit(t, gitwt.New(root), "init", "--bare", "--initial-branch=main", origin)

	a = cloneFor(t, origin, filepath.Join(root, "a"))
	commitFile(t, a, "seed.md", "seed")
	mustGit(t, a, "push", "--set-upstream", "origin", "main")

	b = cloneFor(t, origin, filepath.Join(root, "b"))
	return a, b
}

func cloneFor(t *testing.T, origin, dir string) *gitwt.Repo {
	t.Helper()
	r := gitwt.New(dir)
	if err := r.Clone(origin); err != nil {
		t.Fatal(err)
	}
	mustGit(t, r, "config", "user.name", "test")
	mustGit(t, r, "config", "user.email", "test@example.com")
	mustGit(t, r, "config", "commit.gpgsign", "false")
	mustGit(t, r, "checkout", "-B", "main")
	return r
}

func commitFile(t *testing.T, r *gitwt.Repo, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.Dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, r, "add", "-A")
	mustGit(t, r, "commit", "-m", "add "+name)
}

func mustGit(t *testing.T, r *gitwt.Repo, args ...string) {
	t.Helper()
	if _, err := r.Run(args...); err != nil {
		t.Fatal(err)
	}
}

func assertIdle(t *testing.T, r *gitwt.Repo, want bool) {
	t.Helper()
	got, err := remoteAndLocalIdle(r, "main")
	if err != nil {
		t.Fatalf("remoteAndLocalIdle: %v", err)
	}
	if got != want {
		t.Fatalf("remoteAndLocalIdle = %v, want %v", got, want)
	}
}

func TestRemoteAndLocalIdle_InSync(t *testing.T) {
	_, b := idleFixture(t)
	assertIdle(t, b, true)
}

func TestRemoteAndLocalIdle_Unpushed(t *testing.T) {
	_, b := idleFixture(t)
	commitFile(t, b, "local.md", "local")
	assertIdle(t, b, false)
}

func TestRemoteAndLocalIdle_RemoteMoved(t *testing.T) {
	a, b := idleFixture(t)
	commitFile(t, a, "other.md", "other")
	mustGit(t, a, "push", "origin", "main")
	assertIdle(t, b, false)
}

// Regression: a fetch advanced refs/remotes/origin/main but HEAD never
// caught up. The old gate saw "remote unchanged, nothing unpushed" and
// stayed idle forever (until another PC pushed).
func TestRemoteAndLocalIdle_FetchedButBehind(t *testing.T) {
	a, b := idleFixture(t)
	commitFile(t, a, "other.md", "other")
	mustGit(t, a, "push", "origin", "main")
	mustGit(t, b, "fetch", "origin")
	assertIdle(t, b, false)
}
