package distill

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestParseFrontmatter(t *testing.T) {
	cases := []struct {
		name      string
		content   string
		wantMeta  map[string]string
		wantBody  string
		checkBody bool
	}{
		{
			name:     "flat keys (older schema)",
			content:  "---\nname: foo\ntype: feedback\nscope: environment\n---\nbody line\n",
			wantMeta: map[string]string{"name": "foo", "type": "feedback", "scope": "environment"},
			wantBody: "body line\n", checkBody: true,
		},
		{
			name:     "nested metadata (current schema)",
			content:  "---\nname: bar\ndescription: a desc\nmetadata:\n  type: user\n  scope: environment\n---\n\nthe body\n",
			wantMeta: map[string]string{"name": "bar", "description": "a desc", "type": "user", "scope": "environment"},
			wantBody: "the body\n", checkBody: true,
		},
		{
			name:     "no frontmatter",
			content:  "just a body\n",
			wantMeta: map[string]string{},
			wantBody: "just a body\n", checkBody: true,
		},
		{
			name:     "unterminated frontmatter is treated as body",
			content:  "---\nname: oops\nno closing fence\n",
			wantMeta: map[string]string{},
		},
		{
			name:     "quoted values stripped",
			content:  "---\nname: \"quoted\"\n---\nx\n",
			wantMeta: map[string]string{"name": "quoted"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta, body, err := parseFrontmatter(tc.content)
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.wantMeta {
				if meta[k] != v {
					t.Errorf("meta[%q] = %q, want %q", k, meta[k], v)
				}
			}
			if len(meta) != len(tc.wantMeta) {
				t.Errorf("meta = %v, want exactly %v", meta, tc.wantMeta)
			}
			if tc.checkBody && body != tc.wantBody {
				t.Errorf("body = %q, want %q", body, tc.wantBody)
			}
		})
	}
}

// catalogEntry builds an entry file body with nested-metadata frontmatter.
func catalogEntry(name, desc, typ, originProject, originFile, body string) string {
	return strings.Join([]string{
		"---",
		"name: " + name,
		"description: " + desc,
		"metadata:",
		"  type: " + typ,
		"  scope: environment",
		"  originProject: " + originProject,
		"  originFile: " + originFile,
		"---",
		"",
		body,
		"",
	}, "\n")
}

func TestBuildIndexWritesSortedIndex(t *testing.T) {
	dir := t.TempDir()
	distilled := filepath.Join(dir, "distilled")
	writeFile(t, filepath.Join(distilled, "zeta.md"), catalogEntry("zeta", "Z lesson", "feedback", "P1", "z.md", "z body"))
	writeFile(t, filepath.Join(distilled, "alpha.md"), catalogEntry("alpha", "A lesson", "user", "P2", "a.md", "a body"))
	// noise that must be ignored
	writeFile(t, filepath.Join(distilled, "alpha.md.tmp.123.abc"), "junk")
	writeFile(t, filepath.Join(distilled, ".hidden.md"), "junk")

	res, err := BuildIndex(Options{DistilledDir: distilled, ProjectsDir: filepath.Join(dir, "nope")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Indexed != 2 {
		t.Fatalf("Indexed = %d, want 2", res.Indexed)
	}
	idx, err := os.ReadFile(filepath.Join(distilled, IndexFileName))
	if err != nil {
		t.Fatal(err)
	}
	s := string(idx)
	ai := strings.Index(s, "alpha")
	zi := strings.Index(s, "zeta")
	if ai < 0 || zi < 0 || ai > zi {
		t.Errorf("index not sorted alpha-before-zeta:\n%s", s)
	}
	if !strings.Contains(s, "[alpha](alpha.md)") {
		t.Errorf("index missing entry link:\n%s", s)
	}
}

func TestBuildIndexCatalogConflict(t *testing.T) {
	dir := t.TempDir()
	distilled := filepath.Join(dir, "distilled")
	writeFile(t, filepath.Join(distilled, "dup-a.md"), catalogEntry("dup", "first", "feedback", "P1", "x.md", "body one"))
	writeFile(t, filepath.Join(distilled, "dup-b.md"), catalogEntry("dup", "second", "feedback", "P2", "y.md", "body TWO differs"))

	res, err := BuildIndex(Options{DistilledDir: distilled})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Conflicts) != 1 || res.Conflicts[0].Name != "dup" {
		t.Fatalf("Conflicts = %+v, want one for 'dup'", res.Conflicts)
	}
}

// sourceMemory builds a project-side memory file carrying the marker.
func sourceMemory(name string, marked bool, body string) string {
	scope := ""
	if marked {
		scope = "  scope: environment\n"
	}
	return "---\nname: " + name + "\ndescription: d\nmetadata:\n  type: feedback\n" + scope + "---\n\n" + body + "\n"
}

func TestBuildIndexPendingWorklist(t *testing.T) {
	dir := t.TempDir()
	projects := filepath.Join(dir, "projects")
	distilled := filepath.Join(dir, "distilled")

	// A marked memory already in the catalog. The source keeps a human-readable
	// name while the catalog entry uses a normalized slug — they must still match
	// by provenance (project/file), NOT by name, or the entry mis-reports pending.
	writeFile(t, filepath.Join(projects, "P1", "memory", "in-catalog.md"), sourceMemory("In Catalog, Human Name", true, "x"))
	writeFile(t, filepath.Join(distilled, "in-catalog.md"), catalogEntry("in-catalog-slug", "d", "feedback", "P1", "in-catalog.md", "x"))
	// A marked memory NOT yet in the catalog -> pending.
	writeFile(t, filepath.Join(projects, "P1", "memory", "pending.md"), sourceMemory("pending-lesson", true, "y"))
	// An unmarked memory -> ignored.
	writeFile(t, filepath.Join(projects, "P2", "memory", "project-only.md"), sourceMemory("project-only", false, "z"))

	res, err := BuildIndex(Options{DistilledDir: distilled, ProjectsDir: projects})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Pending) != 1 || res.Pending[0].Name != "pending-lesson" {
		t.Fatalf("Pending = %+v, want one for 'pending-lesson'", res.Pending)
	}
}

func TestReconcilePrunesStaleEntries(t *testing.T) {
	dir := t.TempDir()
	projects := filepath.Join(dir, "projects")
	distilled := filepath.Join(dir, "distilled")

	// Entry whose source still carries the marker -> kept.
	writeFile(t, filepath.Join(projects, "P1", "memory", "live.md"), sourceMemory("live", true, "x"))
	writeFile(t, filepath.Join(distilled, "live.md"), catalogEntry("live", "d", "feedback", "P1", "live.md", "x"))
	// Entry whose source lost the marker -> pruned.
	writeFile(t, filepath.Join(projects, "P1", "memory", "untagged.md"), sourceMemory("untagged", false, "y"))
	writeFile(t, filepath.Join(distilled, "untagged.md"), catalogEntry("untagged", "d", "feedback", "P1", "untagged.md", "y"))
	// Entry whose source file is gone -> pruned.
	writeFile(t, filepath.Join(distilled, "gone.md"), catalogEntry("gone", "d", "feedback", "P1", "missing.md", "z"))

	res, err := Reconcile(Options{DistilledDir: distilled, ProjectsDir: projects})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pruned != 2 {
		t.Fatalf("Pruned = %d, want 2", res.Pruned)
	}
	if _, err := os.Stat(filepath.Join(distilled, "live.md")); err != nil {
		t.Errorf("live.md should have survived: %v", err)
	}
	for _, gone := range []string{"untagged.md", "gone.md"} {
		if _, err := os.Stat(filepath.Join(distilled, gone)); !os.IsNotExist(err) {
			t.Errorf("%s should have been pruned", gone)
		}
	}
}

func TestReconcileNeverPrunesBlindWhenSourcesInvisible(t *testing.T) {
	dir := t.TempDir()
	distilled := filepath.Join(dir, "distilled")
	writeFile(t, filepath.Join(distilled, "live.md"), catalogEntry("live", "d", "feedback", "P1", "live.md", "x"))

	res, err := Reconcile(Options{DistilledDir: distilled, ProjectsDir: filepath.Join(dir, "does-not-exist")})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pruned != 0 {
		t.Fatalf("Pruned = %d, want 0 when projects tree is missing", res.Pruned)
	}
	if _, err := os.Stat(filepath.Join(distilled, "live.md")); err != nil {
		t.Errorf("live.md should not be pruned blind: %v", err)
	}
}
