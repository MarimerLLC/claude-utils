package project

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeRemoteURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"git@github.com:Acme/app.git", "github.com/acme/app"},
		{"https://github.com/acme/app", "github.com/acme/app"},
		{"https://github.com/acme/app.git", "github.com/acme/app"},
		{"https://github.com/acme/app/", "github.com/acme/app"},
		{"ssh://git@github.com/acme/app.git", "github.com/acme/app"},
		{"ssh://git@github.com:22/acme/app.git", "github.com/acme/app"},
		{"https://user:token@github.com/acme/app.git", "github.com/acme/app"},
		{"git@gitlab.example.com:group/sub/repo.git", "gitlab.example.com/group/sub/repo"},
	}
	for _, c := range cases {
		if got := NormalizeRemoteURL(c.in); got != c.want {
			t.Errorf("NormalizeRemoteURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalizeRemoteURL_EquivalentFormsCollapse(t *testing.T) {
	forms := []string{
		"git@github.com:Acme/app.git",
		"https://github.com/acme/app",
		"https://github.com/acme/app.git",
		"ssh://git@github.com/Acme/app.git",
	}
	want := NormalizeRemoteURL(forms[0])
	for _, f := range forms[1:] {
		if got := NormalizeRemoteURL(f); got != want {
			t.Errorf("form %q normalized to %q, want %q", f, got, want)
		}
	}
}

func TestIsFingerprintKey(t *testing.T) {
	yes := []string{"g-abc123", "r-deadbeef"}
	no := []string{"-Users-me-code-foo", "proj1", "-home-matta-repos-foo"}
	for _, s := range yes {
		if !IsFingerprintKey(s) {
			t.Errorf("IsFingerprintKey(%q) = false, want true", s)
		}
	}
	for _, s := range no {
		if IsFingerprintKey(s) {
			t.Errorf("IsFingerprintKey(%q) = true, want false", s)
		}
	}
}

// gitCmd runs git in dir, failing the test on error.
func gitCmd(t *testing.T, dir string, args ...string) {
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

func initRepoWithCommit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0600); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-q", "-m", "init")
}

func TestFingerprint_RemoteURL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	initRepoWithCommit(t, dir)
	gitCmd(t, dir, "remote", "add", "origin", "git@github.com:Acme/app.git")

	key, method := Fingerprint(dir, "-fallback")
	if method != MethodRemote {
		t.Fatalf("method = %q, want remote", method)
	}
	if !strings.HasPrefix(key, "g-") {
		t.Errorf("key = %q, want g- prefix", key)
	}

	// A different URL spelling for the same repo yields the same key.
	dir2 := filepath.Join(t.TempDir(), "repo2")
	initRepoWithCommit(t, dir2)
	gitCmd(t, dir2, "remote", "add", "origin", "https://github.com/acme/app")
	key2, _ := Fingerprint(dir2, "-other")
	if key != key2 {
		t.Errorf("equivalent remotes gave different keys: %q vs %q", key, key2)
	}
}

func TestFingerprint_RootCommitFallback(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	initRepoWithCommit(t, dir)
	// No remote configured → root-commit fallback.

	key, method := Fingerprint(dir, "-fallback")
	if method != MethodRoot {
		t.Fatalf("method = %q, want root", method)
	}
	if !strings.HasPrefix(key, "r-") {
		t.Errorf("key = %q, want r- prefix", key)
	}

	// A clone shares the root commit, so it fingerprints identically. (Cloning
	// auto-adds an origin pointing at the local source; drop it so the
	// root-commit path is exercised on both sides.)
	clone := filepath.Join(t.TempDir(), "clone")
	gitCmd(t, t.TempDir(), "clone", "-q", dir, clone)
	gitCmd(t, clone, "remote", "remove", "origin")
	key2, method2 := Fingerprint(clone, "-clone")
	if method2 != MethodRoot || key2 != key {
		t.Errorf("clone key/method = %q/%q, want %q/root", key2, method2, key)
	}
}

func TestFingerprint_PathFallbackForNonGit(t *testing.T) {
	dir := t.TempDir() // not a git repo
	key, method := Fingerprint(dir, "-Users-me-code-foo")
	if method != MethodPath {
		t.Fatalf("method = %q, want path", method)
	}
	if key != "-Users-me-code-foo" {
		t.Errorf("key = %q, want the localHash fallback", key)
	}
}

func TestFingerprint_EmptyPathIsPathFallback(t *testing.T) {
	key, method := Fingerprint("", "-Users-me-code-foo")
	if method != MethodPath || key != "-Users-me-code-foo" {
		t.Errorf("empty path: key/method = %q/%q, want fallback/path", key, method)
	}
}
