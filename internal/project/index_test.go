package project

import (
	"os"
	"path/filepath"
	"testing"
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

	idx, err := Build(projects)
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
	key := idx.KeyFor(projects, "-plain")
	if key != "-plain" {
		t.Fatalf("KeyFor = %q; want path fallback -plain", key)
	}
	if _, ok := idx.Get("-plain"); !ok {
		t.Errorf("KeyFor should cache the computed entry")
	}
}
