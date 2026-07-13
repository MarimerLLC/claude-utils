package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/project"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// makeRepo creates a git repo at dir with a commit and the given origin URL.
func makeRepo(t *testing.T, dir, origin string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", "c")
	gitRun(t, dir, "remote", "add", "origin", origin)
}

// makeClaudeProject creates a Claude project dir (localHash) whose transcript
// records cwd, so project.Build can resolve and fingerprint it.
func makeClaudeProject(t *testing.T, claudeRoot, localHash, cwd string) {
	t.Helper()
	dir := filepath.Join(claudeRoot, localHash)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"user","cwd":"` + filepath.ToSlash(cwd) + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestReconcile_TwoPathsSameRepoUnionInMirror simulates the same repo checked
// out at two different paths (as it would be across machines): two Claude
// projects with different localHashes but the same origin URL resolve to one
// mirror key, and their MEMORY.md sections union there.
func TestReconcile_TwoPathsSameRepoUnionInMirror(t *testing.T) {
	r, root := reconcileFixture(t)

	repoA := filepath.Join(root, "macos", "app")
	repoB := filepath.Join(root, "linux", "app")
	makeRepo(t, repoA, "git@github.com:acme/app.git")
	makeRepo(t, repoB, "https://github.com/acme/app.git") // equivalent URL

	makeClaudeProject(t, r.Claude, "-Users-me-app", repoA)
	makeClaudeProject(t, r.Claude, "-home-matta-app", repoB)
	writeMem(t, r.Claude, "-Users-me-app", "MEMORY.md", "# App\n\n## FromMac\nmac\n")
	writeMem(t, r.Claude, "-home-matta-app", "MEMORY.md", "# App\n\n## FromLinux\nlinux\n")

	idx, err := project.Build(r.Claude)
	if err != nil {
		t.Fatal(err)
	}
	// Both projects must resolve to the same fingerprint key.
	ea, _ := idx.Get("-Users-me-app")
	eb, _ := idx.Get("-home-matta-app")
	if ea.Key == "" || ea.Key != eb.Key {
		t.Fatalf("expected one shared key; got %q and %q", ea.Key, eb.Key)
	}

	if _, err := Reconcile(r, idx, NewManifest()); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(r.Mirror, ea.Key, "memory", "MEMORY.md"))
	if err != nil {
		t.Fatalf("mirror MEMORY.md missing under key %q: %v", ea.Key, err)
	}
	s := string(got)
	if !contains(s, "## FromMac") || !contains(s, "## FromLinux") {
		t.Errorf("mirror MEMORY.md did not union both machines' sections; got %q", s)
	}
	// The legacy per-path dirs must not exist in the mirror.
	if _, err := os.Stat(filepath.Join(r.Mirror, "-Users-me-app")); err == nil {
		t.Errorf("mirror should be keyed by fingerprint, not the local path")
	}
}

// TestMigrateLayout_CollapsesLegacyDirs seeds a legacy path-keyed mirror dir and
// a colliding fingerprint-keyed dir (as if another machine already migrated),
// then asserts they union under the fingerprint key and the legacy dir is gone.
func TestMigrateLayout_CollapsesLegacyDirs(t *testing.T) {
	r, root := reconcileFixture(t)
	repo := filepath.Join(root, "app")
	makeRepo(t, repo, "git@github.com:acme/app.git")
	makeClaudeProject(t, r.Claude, "-home-matta-app", repo)

	idx, err := project.Build(r.Claude)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := idx.Get("-home-matta-app")
	key := entry.Key
	if !project.IsFingerprintKey(key) {
		t.Fatalf("expected a fingerprint key, got %q", key)
	}

	// Legacy path-keyed dir from before the upgrade on this machine.
	writeMem(t, r.Mirror, "-home-matta-app", "MEMORY.md", "# App\n\n## Legacy\nold\n")
	// Fingerprint-keyed dir already present (other machine migrated + pushed).
	writeMem(t, r.Mirror, key, "MEMORY.md", "# App\n\n## Other\nnew\n")

	rep, err := MigrateLayout(r, idx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Merged) != 1 {
		t.Errorf("expected one merged file; got %#v", rep)
	}
	got, err := os.ReadFile(filepath.Join(r.Mirror, key, "memory", "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if s := string(got); !contains(s, "## Legacy") || !contains(s, "## Other") {
		t.Errorf("migration did not union sections; got %q", s)
	}
	if _, err := os.Stat(filepath.Join(r.Mirror, "-home-matta-app")); err == nil {
		t.Errorf("legacy mirror dir should have been removed")
	}

	// Idempotent: a second run has nothing to migrate and must not error.
	rep2, err := MigrateLayout(r, idx)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep2.Merged)+len(rep2.CopiedToMirror)+len(rep2.BackedUp) != 0 {
		t.Errorf("re-running migration should be a no-op; got %#v", rep2)
	}
}
