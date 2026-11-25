package project

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// realTempDir returns t.TempDir() with symlinks and Windows 8.3 short names
// resolved: DecodePath walks real directory names from the root, and macOS
// /var and Windows runners' RUNNER~1 would otherwise not match.
func realTempDir(t *testing.T) string {
	t.Helper()
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mkdirs(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
}

func samePath(a, b string) bool {
	if runtime.GOOS == "linux" {
		return a == b
	}
	return strings.EqualFold(a, b)
}

func TestEncodePath(t *testing.T) {
	cases := map[string]string{
		`S:\src\rdl\csla`:                       "S--src-rdl-csla",
		`c:\`:                                   "c--",
		`S:\src\rdl\csla\.claude\worktrees\x-1`: "S--src-rdl-csla--claude-worktrees-x-1",
		`C:\Users\me\VS Live\2026`:              "C--Users-me-VS-Live-2026",
		"/Users/me/code/foo":                    "-Users-me-code-foo",
		"/home/me/my.site_v2":                   "-home-me-my-site-v2",
	}
	for in, want := range cases {
		if got := EncodePath(in); got != want {
			t.Errorf("EncodePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDecodePath_FindsTheOneMatchingDir(t *testing.T) {
	base := realTempDir(t)
	want := filepath.Join(base, "src", "rdl", "mcp-aggregator") // dash inside a name
	mkdirs(t, want, filepath.Join(base, "src", "rdl", "other"))

	got, ok := DecodePath(EncodePath(want))
	if !ok || !samePath(got, want) {
		t.Fatalf("DecodePath = %q, %v; want %q", got, ok, want)
	}
}

func TestDecodePath_DotAndSpaceSegments(t *testing.T) {
	base := realTempDir(t)
	want := filepath.Join(base, "repo", ".claude", "worktrees", "VS Live")
	mkdirs(t, want)

	got, ok := DecodePath(EncodePath(want))
	if !ok || !samePath(got, want) {
		t.Fatalf("DecodePath = %q, %v; want %q", got, ok, want)
	}
}

// Two existing dirs encode to the same name: refuse to guess.
func TestDecodePath_AmbiguousFails(t *testing.T) {
	base := realTempDir(t)
	a := filepath.Join(base, "x", "a-b")
	b := filepath.Join(base, "x", "a", "b")
	mkdirs(t, a, b)
	if EncodePath(a) != EncodePath(b) {
		t.Fatalf("fixture: %q and %q should encode alike", a, b)
	}
	if got, ok := DecodePath(EncodePath(a)); ok {
		t.Fatalf("DecodePath = %q, want no match for an ambiguous name", got)
	}
}

func TestDecodePath_MissingFails(t *testing.T) {
	base := realTempDir(t)
	if got, ok := DecodePath(EncodePath(filepath.Join(base, "gone", "repo"))); ok {
		t.Fatalf("DecodePath = %q, want no match", got)
	}
	if _, ok := DecodePath("not-an-encoded-path"); ok && runtime.GOOS == "windows" {
		t.Fatal("a name without a drive prefix must not decode on Windows")
	}
}
