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
		{"https://github.com:443/acme/app", "github.com/acme/app"},
		{"ssh://git@git.example:2222/acme/app.git", "git.example:2222/acme/app"},
		{"ssh://git@git.example:2223/acme/app.git", "git.example:2223/acme/app"},
		{"https://git.example:8443/acme/app", "git.example:8443/acme/app"},
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
	yes := []string{"g-0123456789ab", "r-deadbeefcafe"}
	no := []string{
		"-Users-me-code-foo", "proj1", "-home-matta-repos-foo",
		"g--src-app",       // Windows project on a lowercase g: drive
		"r--repos-x",       // ... and on r:
		"g-abc123",         // wrong length
		"g-0123456789AB",   // uppercase is not ours
		"g-0123456789abcd", // too long
	}
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

// A Claude project opened in a subdirectory of a repo is a distinct project
// from the repo root, and a plain folder nested in some enclosing repo must not
// take on that repo's root identity.
func TestFingerprint_SubdirectoryGetsOwnKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	initRepoWithCommit(t, dir)
	gitCmd(t, dir, "remote", "add", "origin", "git@github.com:acme/app.git")
	sub := filepath.Join(dir, "docs", "Guide")
	if err := os.MkdirAll(sub, 0700); err != nil {
		t.Fatal(err)
	}

	rootKey, _ := Fingerprint(dir, "-root")
	subKey, method := Fingerprint(sub, "-sub")
	if method != MethodRemote {
		t.Fatalf("subdir method = %q, want remote", method)
	}
	if subKey == rootKey {
		t.Fatalf("subdirectory shares the repo root's key %q", rootKey)
	}
	if !IsFingerprintKey(subKey) {
		t.Errorf("subdir key %q is not a fingerprint key", subKey)
	}

	// The same subdirectory of another clone (different absolute path) matches.
	dir2 := filepath.Join(t.TempDir(), "elsewhere")
	initRepoWithCommit(t, dir2)
	gitCmd(t, dir2, "remote", "add", "origin", "https://github.com/acme/app")
	sub2 := filepath.Join(dir2, "docs", "Guide")
	if err := os.MkdirAll(sub2, 0700); err != nil {
		t.Fatal(err)
	}
	if k, _ := Fingerprint(sub2, "-sub2"); k != subKey {
		t.Errorf("same subdir in another clone: key %q, want %q", k, subKey)
	}

	// On a case-insensitive filesystem the cwd may be recorded with different
	// casing than the directory has on disk; the key follows the disk.
	typed := filepath.Join(dir2, "DOCS", "guide")
	if _, err := os.Stat(typed); err == nil {
		if k, _ := Fingerprint(typed, "-typed"); k != subKey {
			t.Errorf("cwd typed as %q: key %q, want %q", typed, k, subKey)
		}
	}
}

// On a case-sensitive filesystem, directories differing only in case are
// distinct projects and must not share a key.
func TestFingerprint_CaseDistinctSubdirsOnCaseSensitiveFS(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "repo")
	initRepoWithCommit(t, dir)
	gitCmd(t, dir, "remote", "add", "origin", "git@github.com:acme/app.git")
	lower := filepath.Join(dir, "docs")
	if err := os.MkdirAll(lower, 0700); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(dir, "Docs")
	if _, err := os.Stat(upper); err == nil {
		t.Skip("case-insensitive filesystem")
	}
	if err := os.MkdirAll(upper, 0700); err != nil {
		t.Fatal(err)
	}
	kl, _ := Fingerprint(lower, "-l")
	ku, _ := Fingerprint(upper, "-u")
	if kl == ku {
		t.Errorf("docs and Docs share key %q", kl)
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
