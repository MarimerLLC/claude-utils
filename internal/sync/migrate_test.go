package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
)

func TestPriorVersionIn_FindsHistoricalBlob(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	p := filepath.Join(dir, "projects", "g-0123456789ab", "memory", "f.md")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	for _, v := range []string{"v1", "v2"} {
		if err := os.WriteFile(p, []byte(v), 0600); err != nil {
			t.Fatal(err)
		}
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-q", "-m", v)
	}

	prior := PriorVersionIn(gitwt.New(dir))
	rel := "g-0123456789ab/memory/f.md"
	if !prior(rel, []byte("v1")) {
		t.Errorf("v1 is in history but was not found")
	}
	if !prior(rel, []byte("v2")) {
		t.Errorf("v2 (current) was not found")
	}
	if prior(rel, []byte("v3")) {
		t.Errorf("v3 was never committed but was reported as prior")
	}
	if prior("g-0123456789ab/memory/other.md", []byte("v1")) {
		t.Errorf("blob at a different path must not count")
	}
}

// A pull that stops on a conflict must be detectable and abortable, and so
// must a rebase an older daemon left behind, so the daemon never stays wedged.
func TestMergeAndRebaseInProgress_AbortRecovers(t *testing.T) {
	root := t.TempDir()
	bare := filepath.Join(root, "remote.git")
	gitRun(t, root, "init", "-q", "--bare", "-b", "main", bare)

	clone := func(name string) string {
		d := filepath.Join(root, name)
		gitRun(t, root, "clone", "-q", bare, d)
		return d
	}
	write := func(dir, content string) {
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}

	a := clone("a")
	write(a, "base\n")
	gitRun(t, a, "add", "-A")
	gitRun(t, a, "commit", "-q", "-m", "base")
	gitRun(t, a, "push", "-q", "origin", "main")

	b := clone("b")
	// repo.Pull runs without the test's identity env; the rebase it does needs
	// a committer identity, which CI runners don't have globally.
	gitRun(t, b, "config", "user.name", "t")
	gitRun(t, b, "config", "user.email", "t@t")
	write(a, "from a\n")
	gitRun(t, a, "commit", "-q", "-am", "a")
	gitRun(t, a, "push", "-q", "origin", "main")
	write(b, "from b\n")
	gitRun(t, b, "commit", "-q", "-am", "b")

	repo := gitwt.New(b)
	if err := repo.Pull("main"); err == nil {
		t.Fatal("expected a conflicting pull to fail")
	}
	if !repo.MergeInProgress() {
		t.Fatal("expected a merge in progress after the conflict")
	}
	if repo.RebaseInProgress() {
		t.Fatal("pull must merge, not rebase")
	}
	if err := repo.AbortMerge(); err != nil {
		t.Fatal(err)
	}
	if repo.MergeInProgress() {
		t.Fatal("merge still in progress after abort")
	}

	// What an older daemon's `pull --rebase` left behind.
	if _, err := repo.Run("rebase", "refs/remotes/origin/main"); err == nil {
		t.Fatal("expected a conflicting rebase to stop")
	}
	if !repo.RebaseInProgress() {
		t.Fatal("expected a rebase in progress after the conflict")
	}
	if err := repo.AbortRebase(); err != nil {
		t.Fatal(err)
	}
	if repo.RebaseInProgress() {
		t.Error("rebase still in progress after abort")
	}
}
