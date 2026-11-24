package project

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, projectDir, cwd string) {
	t.Helper()
	if err := os.MkdirAll(projectDir, 0700); err != nil {
		t.Fatal(err)
	}
	// json-encode the cwd so backslashes/quotes are handled.
	b, err := os.OpenFile(filepath.Join(projectDir, "s.jsonl"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.WriteString(`{"type":"user","cwd":` + quoteJSON(cwd) + "}\n"); err != nil {
		t.Fatal(err)
	}
}

func quoteJSON(s string) string {
	out := `"`
	for _, r := range s {
		switch r {
		case '\\':
			out += `\\`
		case '"':
			out += `\"`
		default:
			out += string(r)
		}
	}
	return out + `"`
}

func TestBuild_MixedProjects(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "app")
	initRepoWithCommit(t, repo)
	gitCmd(t, repo, "remote", "add", "origin", "git@github.com:acme/app.git")

	projects := t.TempDir()
	gitProj := filepath.Join(projects, "-fake-localhash")
	writeTranscript(t, gitProj, repo)

	plain := filepath.Join(projects, "-plain-dir")
	if err := os.MkdirAll(plain, 0700); err != nil { // no transcript → path fallback
		t.Fatal(err)
	}

	idx, err := Build(projects, nil)
	if err != nil {
		t.Fatal(err)
	}

	gitEntry, ok := idx.Get("-fake-localhash")
	if !ok || gitEntry.Method != MethodRemote {
		t.Fatalf("git project entry = %+v, ok=%v; want remote", gitEntry, ok)
	}
	if locals := idx.LocalsFor(gitEntry.Key); len(locals) != 1 || locals[0] != "-fake-localhash" {
		t.Errorf("LocalsFor(%q) = %v; want [-fake-localhash]", gitEntry.Key, locals)
	}

	plainEntry, ok := idx.Get("-plain-dir")
	if !ok || plainEntry.Method != MethodPath || plainEntry.Key != "-plain-dir" {
		t.Errorf("plain entry = %+v; want path fallback keyed by its own name", plainEntry)
	}
}

func TestIndex_SaveLoadRoundTrip(t *testing.T) {
	syncDir := t.TempDir()
	idx := NewIndex()
	idx.byLocal["-a"] = Entry{Key: "g-abc", Method: MethodRemote, Path: "/x/a"}
	idx.byLocal["-b"] = Entry{Key: "-b", Method: MethodPath}
	if err := idx.Save(syncDir); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(syncDir)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := loaded.Get("-a"); !ok || e.Key != "g-abc" || e.Method != MethodRemote {
		t.Errorf("round-trip lost -a: %+v ok=%v", e, ok)
	}
	if e, ok := loaded.Get("-b"); !ok || e.Key != "-b" {
		t.Errorf("round-trip lost -b: %+v ok=%v", e, ok)
	}
}

func TestLoad_MissingReturnsEmpty(t *testing.T) {
	idx, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("missing index must not error; got %v", err)
	}
	if len(idx.All()) != 0 {
		t.Errorf("expected empty index")
	}
}

func TestKeyFor_CachesOnMiss(t *testing.T) {
	projects := t.TempDir()
	plain := filepath.Join(projects, "-plain")
	if err := os.MkdirAll(plain, 0700); err != nil {
		t.Fatal(err)
	}
	idx := NewIndex()
	key, ok := idx.KeyFor(projects, "-plain")
	if !ok || key != "-plain" {
		t.Fatalf("KeyFor = %q, %v; want path fallback -plain", key, ok)
	}
	if _, ok := idx.Get("-plain"); !ok {
		t.Errorf("KeyFor should cache the computed entry")
	}
}

// A project whose transcript was just created but doesn't carry a cwd yet must
// not be pinned to the path key: KeyFor reports pending and caches nothing, and
// Build leaves it out.
func TestKeyFor_PendingWhenTranscriptLacksCwd(t *testing.T) {
	projects := t.TempDir()
	dir := filepath.Join(projects, "-new-proj")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "s.jsonl"), []byte(`{"type":"mode"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	idx := NewIndex()
	if key, ok := idx.KeyFor(projects, "-new-proj"); ok {
		t.Fatalf("KeyFor = %q, true; want pending", key)
	}
	if _, cached := idx.Get("-new-proj"); cached {
		t.Errorf("pending project must not be cached")
	}
	built, err := Build(projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := built.Get("-new-proj"); ok {
		t.Errorf("Build must leave a pending project out")
	}

	// Once the transcript ages past the window, the path key is accepted.
	old := time.Now().Add(-2 * pendingWindow)
	if err := os.Chtimes(filepath.Join(dir, "s.jsonl"), old, old); err != nil {
		t.Fatal(err)
	}
	if key, ok := idx.KeyFor(projects, "-new-proj"); !ok || key != "-new-proj" {
		t.Errorf("stale transcript: KeyFor = %q, %v; want path fallback", key, ok)
	}
}

// When the checkout a project was fingerprinted from disappears (removed
// worktree, transcripts cleaned up), the project keeps its last key instead of
// re-keying to the path.
func TestBuild_KeepsKeyWhenCheckoutGone(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "wt")
	initRepoWithCommit(t, repo)
	gitCmd(t, repo, "remote", "add", "origin", "git@github.com:acme/app.git")
	projects := t.TempDir()
	writeTranscript(t, filepath.Join(projects, "-wt"), repo)

	first, err := Build(projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := first.Get("-wt")
	if want.Method != MethodRemote {
		t.Fatalf("setup: method = %q", want.Method)
	}

	if err := os.RemoveAll(repo); err != nil {
		t.Fatal(err)
	}
	// Touch the transcript so the stamp changes and the entry is re-resolved.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(projects, "-wt", "s.jsonl"), later, later); err != nil {
		t.Fatal(err)
	}
	second, err := Build(projects, first)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := second.Get("-wt")
	if got.Key != want.Key {
		t.Errorf("key after checkout removed = %q, want kept %q", got.Key, want.Key)
	}

	// Without a previous index there's nothing to keep: path fallback.
	fresh, err := Build(projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := fresh.Get("-wt"); e.Key != "-wt" {
		t.Errorf("no prev index: key = %q, want path fallback", e.Key)
	}
}

// An unchanged transcript stamp reuses the previous entry without re-running
// git (observable here because the reused entry wins over what git would say).
func TestBuild_ReusesEntryWhenStampUnchanged(t *testing.T) {
	projects := t.TempDir()
	writeTranscript(t, filepath.Join(projects, "-p"), t.TempDir())
	prev := NewIndex()
	prev.byLocal["-p"] = Entry{Key: "g-0123456789ab", Method: MethodRemote, Stamp: TranscriptStamp(filepath.Join(projects, "-p"))}

	idx, err := Build(projects, prev)
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := idx.Get("-p"); e.Key != "g-0123456789ab" {
		t.Errorf("key = %q; want reused entry", e.Key)
	}
}

// Old daemon versions copied fingerprint-keyed mirror dirs into Claude's tree
// verbatim; those are not projects and must not join the index.
func TestBuild_SkipsFingerprintNamedDirs(t *testing.T) {
	projects := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projects, "g-0123456789ab", "memory"), 0700); err != nil {
		t.Fatal(err)
	}
	idx, err := Build(projects, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.All()) != 0 {
		t.Errorf("fingerprint-named dir was indexed: %v", idx.All())
	}
}

func TestLoad_IgnoresOtherVersion(t *testing.T) {
	syncDir := t.TempDir()
	p := IndexPath(syncDir)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"version":1,"entries":{"-a":{"key":"g-x","method":"remote"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	idx, err := Load(syncDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(idx.All()) != 0 {
		t.Errorf("version-1 entries should be ignored; got %v", idx.All())
	}
}
