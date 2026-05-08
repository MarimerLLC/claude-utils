package sync

import (
	"os"
	"path/filepath"
	"testing"
)

// reconcileFixture builds a temp Claude tree and mirror tree under a single
// test root, returning the Roots to feed into Reconcile.
func reconcileFixture(t *testing.T) (Roots, string) {
	t.Helper()
	root := t.TempDir()
	claude := filepath.Join(root, "claude")
	mirror := filepath.Join(root, "mirror")
	for _, p := range []string{claude, mirror} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	return Roots{Claude: claude, Mirror: mirror}, root
}

func writeMem(t *testing.T, root, hash, name, content string) {
	t.Helper()
	dir := filepath.Join(root, hash, "memory")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func memExists(t *testing.T, root, hash, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(root, hash, "memory", name))
	return err == nil
}

func TestReconcile_DeleteOnThisPC_PropagatesToMirror(t *testing.T) {
	r, _ := reconcileFixture(t)
	// Last sync had this file; it's still in the mirror but the user
	// deleted it from Claude while the daemon was off.
	writeMem(t, r.Mirror, "proj1", "feedback_x.md", "old content")

	manifest := NewManifest()
	manifest.Add("proj1/memory/feedback_x.md")

	rep, err := Reconcile(r, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if memExists(t, r.Mirror, "proj1", "feedback_x.md") {
		t.Errorf("expected mirror file removed; still present")
	}
	if memExists(t, r.Claude, "proj1", "feedback_x.md") {
		t.Errorf("Claude side must not be re-created")
	}
	if len(rep.RemovedFromMirror) != 1 || rep.RemovedFromMirror[0] != "proj1/memory/feedback_x.md" {
		t.Errorf("expected RemovedFromMirror to list the file; got %#v", rep.RemovedFromMirror)
	}
}

func TestReconcile_NewFromAnotherPC_CopiesToClaude(t *testing.T) {
	r, _ := reconcileFixture(t)
	// Another PC pushed this file; we just pulled it. It's in mirror but
	// not in Claude AND not in our manifest.
	writeMem(t, r.Mirror, "proj1", "feedback_x.md", "from PC2")
	manifest := NewManifest()

	rep, err := Reconcile(r, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !memExists(t, r.Claude, "proj1", "feedback_x.md") {
		t.Errorf("expected file copied to Claude")
	}
	if len(rep.CopiedToClaude) != 1 {
		t.Errorf("expected CopiedToClaude entry; got %#v", rep)
	}
	if len(rep.RemovedFromMirror) != 0 {
		t.Errorf("must not delete: %#v", rep.RemovedFromMirror)
	}
}

func TestReconcile_NilManifest_NeverDeletes(t *testing.T) {
	r, _ := reconcileFixture(t)
	// Same shape as the delete test, but caller passes nil manifest
	// (first-run / missing manifest case).
	writeMem(t, r.Mirror, "proj1", "feedback_x.md", "old content")

	rep, err := Reconcile(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Without manifest knowledge we must not infer a delete; treat as
	// "new from another PC" and copy to Claude.
	if !memExists(t, r.Claude, "proj1", "feedback_x.md") {
		t.Errorf("expected file copied to Claude (safe default)")
	}
	if memExists(t, r.Mirror, "proj1", "feedback_x.md") == false {
		t.Errorf("mirror file must remain")
	}
	if len(rep.RemovedFromMirror) != 0 {
		t.Errorf("nil manifest must never produce deletes; got %#v", rep.RemovedFromMirror)
	}
}

func TestReconcile_NewLocalFile_CopiesToMirror(t *testing.T) {
	r, _ := reconcileFixture(t)
	writeMem(t, r.Claude, "proj1", "MEMORY.md", "# new on this PC\n")
	manifest := NewManifest()

	rep, err := Reconcile(r, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !memExists(t, r.Mirror, "proj1", "MEMORY.md") {
		t.Errorf("expected file copied to mirror")
	}
	if len(rep.CopiedToMirror) != 1 {
		t.Errorf("expected CopiedToMirror entry; got %#v", rep)
	}
}

func TestReconcile_BothPresentSemanticMerge(t *testing.T) {
	r, _ := reconcileFixture(t)
	writeMem(t, r.Claude, "proj1", "MEMORY.md", "# Project\n\n## Local\nfrom Claude\n")
	writeMem(t, r.Mirror, "proj1", "MEMORY.md", "# Project\n\n## Remote\nfrom mirror\n")
	manifest := NewManifest()

	rep, err := Reconcile(r, manifest)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(r.Claude, "proj1", "memory", "MEMORY.md"))
	s := string(got)
	if !contains(s, "## Local") || !contains(s, "## Remote") {
		t.Errorf("expected semantic merge to keep both sections; got %q", s)
	}
	if len(rep.Merged) != 1 {
		t.Errorf("expected Merged entry; got %#v", rep)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestManifest_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := NewManifest()
	m.Add("a/memory/MEMORY.md")
	m.Add("b/memory/feedback.md")
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Has("a/memory/MEMORY.md") || !loaded.Has("b/memory/feedback.md") {
		t.Errorf("round-trip lost entries: have %d", loaded.Len())
	}
	if loaded.Has("nonexistent") {
		t.Errorf("phantom entry")
	}
}

func TestLoadManifest_MissingReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("missing manifest must not error; got %v", err)
	}
	if m.Len() != 0 {
		t.Errorf("expected empty manifest; got %d entries", m.Len())
	}
}

func TestBuildFromClaudeTree(t *testing.T) {
	root := t.TempDir()
	writeMem(t, root, "p1", "MEMORY.md", "x")
	writeMem(t, root, "p1", "feedback_a.md", "y")
	writeMem(t, root, "p2", "MEMORY.md", "z")

	m, err := BuildFromClaudeTree(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"p1/memory/MEMORY.md", "p1/memory/feedback_a.md", "p2/memory/MEMORY.md"}
	for _, w := range want {
		if !m.Has(w) {
			t.Errorf("missing %q", w)
		}
	}
	if m.Len() != len(want) {
		t.Errorf("len=%d want %d", m.Len(), len(want))
	}
}
