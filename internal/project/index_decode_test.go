package project

import (
	"os"
	"path/filepath"
	"testing"
)

// decodableRepo creates a git repo with an origin at a real (symlink-free) path
// and returns it with the Claude project folder name that encodes it.
func decodableRepo(t *testing.T, origin string) (repo, localHash string) {
	t.Helper()
	repo = filepath.Join(realTempDir(t), "src", "my-app")
	initRepoWithCommit(t, repo)
	gitCmd(t, repo, "remote", "add", "origin", origin)
	return repo, EncodePath(repo)
}

// A project folder sync materialized for a repo never opened here has no
// transcript; its name still decodes to the checkout, so it fingerprints like
// it does on every other PC instead of staying on the path key.
func TestBuild_DecodesProjectWithoutTranscript(t *testing.T) {
	repo, lh := decodableRepo(t, "git@github.com:acme/app.git")
	projects := t.TempDir()
	mkdirs(t, filepath.Join(projects, lh, "memory"))

	idx, err := Build(projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Fingerprint(repo, lh)
	e, _ := idx.Get(lh)
	if e.Method != MethodRemote || e.Key != want {
		t.Fatalf("entry = %+v; want remote key %q", e, want)
	}
}

// A transcript naming a directory that no longer exists (a renamed checkout)
// falls back to decoding the folder name.
func TestBuild_DecodesWhenRecordedCwdIsGone(t *testing.T) {
	repo, lh := decodableRepo(t, "git@github.com:acme/app.git")
	projects := t.TempDir()
	writeTranscript(t, filepath.Join(projects, lh), filepath.Join(t.TempDir(), "renamed-away"))

	idx, err := Build(projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Fingerprint(repo, lh)
	if e, _ := idx.Get(lh); e.Key != want {
		t.Fatalf("key = %q; want %q", e.Key, want)
	}
}

// A path entry cached from an older version (unchanged transcript stamp) must
// not short-circuit the new decoding.
func TestBuild_ReResolvesCachedPathEntries(t *testing.T) {
	repo, lh := decodableRepo(t, "git@github.com:acme/app.git")
	projects := t.TempDir()
	dir := filepath.Join(projects, lh)
	writeTranscript(t, dir, filepath.Join(t.TempDir(), "gone"))
	prev := NewIndex()
	prev.byLocal[lh] = Entry{Key: lh, Method: MethodPath, Stamp: TranscriptStamp(dir)}

	idx, err := Build(projects, prev)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := Fingerprint(repo, lh)
	if e, _ := idx.Get(lh); e.Key != want {
		t.Fatalf("key = %q; want %q (cached path entry re-resolved)", e.Key, want)
	}
}

// Once decoded, the key stays put if the checkout later disappears: the
// project must not fall back to the path key and split off again.
func TestBuild_DecodedKeyStaysWhenCheckoutGone(t *testing.T) {
	repo, lh := decodableRepo(t, "git@github.com:acme/app.git")
	projects := t.TempDir()
	mkdirs(t, filepath.Join(projects, lh, "memory"))
	first, err := Build(projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := first.Get(lh)

	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	second, err := Build(projects, first)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := second.Get(lh); e.Key != want.Key {
		t.Fatalf("key after checkout removed = %q; want kept %q", e.Key, want.Key)
	}
}

// A project that followed a redirect (MethodMoved) keeps that key across
// rebuilds even though Build alone would key it by path.
func TestBuild_KeepsMovedEntry(t *testing.T) {
	projects := t.TempDir()
	lh := "-nowhere-on-this-pc"
	mkdirs(t, filepath.Join(projects, lh, "memory"))
	prev := NewIndex()
	prev.byLocal[lh] = Entry{Key: "g-0123456789ab", Method: MethodMoved}

	idx, err := Build(projects, prev)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := idx.Get(lh); e.Key != "g-0123456789ab" || e.Method != MethodMoved {
		t.Fatalf("entry = %+v; want the moved entry kept", e)
	}
}
