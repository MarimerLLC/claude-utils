package sync

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/gitwt"
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

	idx, err := project.Build(r.Claude, nil)
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

// fingerprintedProject sets up one git-backed Claude project and returns its
// index and key.
func fingerprintedProject(t *testing.T, r Roots, root, localHash string) (*project.Index, string) {
	t.Helper()
	repo := filepath.Join(root, "repo-"+localHash)
	makeRepo(t, repo, "git@github.com:acme/app.git")
	makeClaudeProject(t, r.Claude, localHash, repo)
	idx, err := project.Build(r.Claude, nil)
	if err != nil {
		t.Fatal(err)
	}
	entry, _ := idx.Get(localHash)
	if !project.IsFingerprintKey(entry.Key) {
		t.Fatalf("expected a fingerprint key, got %q", entry.Key)
	}
	return idx, entry.Key
}

func readMem(t *testing.T, root, hash, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, hash, "memory", name))
	if err != nil {
		t.Fatalf("read %s/%s: %v", hash, name, err)
	}
	return string(b)
}

// TestMigrateLayout_UnionsAndKeepsLegacyDir seeds a legacy path-keyed mirror
// dir and a colliding fingerprint-keyed dir (as if another machine already
// migrated), then asserts they union under the fingerprint key — and that the
// legacy dir is left in place, since a PC still on the old version would read
// its deletion as the user deleting those memories.
func TestMigrateLayout_UnionsAndKeepsLegacyDir(t *testing.T) {
	r, root := reconcileFixture(t)
	idx, key := fingerprintedProject(t, r, root, "-home-matta-app")

	writeMem(t, r.Mirror, "-home-matta-app", "MEMORY.md", "# App\n\n## Legacy\nold\n")
	writeMem(t, r.Mirror, key, "MEMORY.md", "# App\n\n## Other\nnew\n")

	state := NewMigrationState()
	rep, err := MigrateLayout(r, idx, MigrateOptions{State: state})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Merged) != 1 {
		t.Errorf("expected one merged file; got %#v", rep)
	}
	if s := readMem(t, r.Mirror, key, "MEMORY.md"); !contains(s, "## Legacy") || !contains(s, "## Other") {
		t.Errorf("migration did not union sections; got %q", s)
	}
	if !memExists(t, r.Mirror, "-home-matta-app", "MEMORY.md") {
		t.Errorf("legacy mirror dir must be kept unless pruning")
	}

	// The user then deletes the Legacy section under the key. Re-running with
	// the same state must not merge the unchanged legacy file back in.
	writeMem(t, r.Mirror, key, "MEMORY.md", "# App\n\n## Other\nnew\n")
	rep2, err := MigrateLayout(r, idx, MigrateOptions{State: state})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rep2.Merged) + len(rep2.CopiedToMirror) + len(rep2.BackedUp); n != 0 {
		t.Errorf("re-running migration should be a no-op; got %#v", rep2)
	}
	if s := readMem(t, r.Mirror, key, "MEMORY.md"); contains(s, "## Legacy") {
		t.Errorf("deleted section resurrected: %q", s)
	}

	// An old-version PC edits the legacy file: that edit is imported.
	writeMem(t, r.Mirror, "-home-matta-app", "MEMORY.md", "# App\n\n## Legacy\nold\n\n## FromOldPC\nx\n")
	if _, err := MigrateLayout(r, idx, MigrateOptions{State: state}); err != nil {
		t.Fatal(err)
	}
	if s := readMem(t, r.Mirror, key, "MEMORY.md"); !contains(s, "## FromOldPC") {
		t.Errorf("edit to the legacy dir was not imported: %q", s)
	}
}

// A legacy file whose content the key's file already held at some point (the
// Prior check) is stale and must not be copied back after a delete or saved as
// a collision backup.
func TestMigrateLayout_SkipsPriorVersions(t *testing.T) {
	r, root := reconcileFixture(t)
	idx, key := fingerprintedProject(t, r, root, "-home-matta-app")
	writeMem(t, r.Mirror, "-home-matta-app", "feedback_a.md", "v1")
	writeMem(t, r.Mirror, "-home-matta-app", "feedback_b.md", "v1")
	writeMem(t, r.Mirror, key, "feedback_a.md", "v2") // edited since import elsewhere
	// feedback_b.md was deleted under the key since import.

	prior := func(rel string, content []byte) bool { return string(content) == "v1" }
	rep, err := MigrateLayout(r, idx, MigrateOptions{State: NewMigrationState(), Prior: prior})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rep.Merged) + len(rep.CopiedToMirror) + len(rep.BackedUp); n != 0 {
		t.Errorf("stale legacy versions should be skipped; got %#v", rep)
	}
	if memExists(t, r.Mirror, key, "feedback_b.md") {
		t.Errorf("deleted file resurrected from a stale legacy copy")
	}
}

// Collision backups in a legacy dir are recovery files, not memories: carried
// over verbatim (never merged), and only then pruned from the legacy dir.
func TestMigrateLayout_CarriesCollisionBackupsVerbatim(t *testing.T) {
	r, root := reconcileFixture(t)
	idx, key := fingerprintedProject(t, r, root, "-home-matta-app")
	const backup = "feedback_a.md.from-remote-0123abcd"
	writeMem(t, r.Mirror, "-home-matta-app", backup, "recovered")

	rep, err := MigrateLayout(r, idx, MigrateOptions{State: NewMigrationState(), Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := readMem(t, r.Mirror, key, backup); got != "recovered" {
		t.Errorf("backup not carried over verbatim: %q", got)
	}
	if n := len(rep.Merged) + len(rep.BackedUp); n != 0 {
		t.Errorf("backup must not be merged or re-backed-up; got %#v", rep)
	}
	if memExists(t, r.Mirror, "-home-matta-app", backup) {
		t.Errorf("legacy copy should be pruned once carried over")
	}
}

// A legacy copy edited after the key's file was imported from it (by a PC on
// an older version, or one whose sync was stuck) is merged into the key's
// file against the version both held — not just saved as a backup, which
// pruning then left as the only copy of the newer text.
func TestMigrateLayout_MergesLegacyEditAgainstSharedBase(t *testing.T) {
	const body = "---\nname: n\n---\n\nl1\nl2\nl3\nl4\nl5\n"
	cases := map[string]struct{ key, legacy, want string }{
		"only legacy edited": {
			key:    body,
			legacy: strings.Replace(body, "l3", "l3 FIXED", 1),
			want:   strings.Replace(body, "l3", "l3 FIXED", 1),
		},
		"both edited apart": {
			key:    strings.Replace(body, "l1", "l1-key", 1),
			legacy: strings.Replace(body, "l5", "l5-legacy", 1),
			want:   strings.Replace(strings.Replace(body, "l1", "l1-key", 1), "l5", "l5-legacy", 1),
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r, root := reconcileFixture(t)
			idx, key := fingerprintedProject(t, r, root, "-home-matta-app")
			writeMem(t, r.Mirror, "-home-matta-app", "note.md", c.legacy)
			writeMem(t, r.Mirror, key, "note.md", c.key)
			base := func(legacyRel, rel string) []byte { return []byte(body) }

			rep, err := MigrateLayout(r, idx, MigrateOptions{State: NewMigrationState(), Base: base, Prune: true})
			if err != nil {
				t.Fatal(err)
			}
			if got := readMem(t, r.Mirror, key, "note.md"); got != c.want {
				t.Fatalf("legacy edit not merged:\n got %q\nwant %q", got, c.want)
			}
			if len(rep.BackedUp) != 0 {
				t.Errorf("a clean merge needs no backup; got %#v", rep)
			}
			if memExists(t, r.Mirror, "-home-matta-app", "note.md") {
				t.Errorf("legacy copy should be pruned once merged")
			}
		})
	}
}

// Overlapping edits with no timestamps: the side changed later in history
// wins, and the other side's original is kept as a backup.
func TestMigrateLayout_OverlapGoesToLaterChange(t *testing.T) {
	for _, legacyLater := range []bool{true, false} {
		t.Run(fmt.Sprintf("legacyLater=%v", legacyLater), func(t *testing.T) {
			r, root := reconcileFixture(t)
			idx, key := fingerprintedProject(t, r, root, "-home-matta-app")
			writeMem(t, r.Mirror, "-home-matta-app", "note.md", "a\nb-legacy\nc\n")
			writeMem(t, r.Mirror, key, "note.md", "a\nb-key\nc\n")

			rep, err := MigrateLayout(r, idx, MigrateOptions{
				State:       NewMigrationState(),
				Base:        func(string, string) []byte { return []byte("a\nb\nc\n") },
				LegacyLater: func(string, string) bool { return legacyLater },
			})
			if err != nil {
				t.Fatal(err)
			}
			win, lose := "a\nb-key\nc\n", "a\nb-legacy\nc\n"
			if legacyLater {
				win, lose = lose, win
			}
			if got := readMem(t, r.Mirror, key, "note.md"); got != win {
				t.Fatalf("got %q, want %q", got, win)
			}
			bk, _ := filepath.Glob(filepath.Join(r.Mirror, key, "memory", "note.md.from-remote-*"))
			if len(bk) != 1 || len(rep.BackedUp) != 1 {
				t.Fatalf("want one backup; got %v, %#v", bk, rep)
			}
			if got, _ := os.ReadFile(bk[0]); string(got) != lose {
				t.Fatalf("backup should hold the losing version %q, got %q", lose, got)
			}
		})
	}
}

// The history-backed helpers: the shared version is the newest one both paths
// held, and the legacy copy counts as changed later after a newer commit.
func TestSharedVersionAndChangedLaterIn(t *testing.T) {
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	write := func(rel, content string) {
		p := filepath.Join(dir, "projects", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg, date string) {
		gitRun(t, dir, "add", "-A")
		gitRun(t, dir, "commit", "-q", "--date="+date, "-m", msg)
	}
	const legacy, key = "S--src-app/memory/f.md", "g-0123456789ab/memory/f.md"
	write(legacy, "v0")
	commit("legacy v0", "2026-08-01T00:00:00Z")
	write(legacy, "v1")
	commit("legacy v1", "2026-09-01T00:00:00Z")
	write(key, "v1") // migrated from the legacy copy
	commit("migrate", "2026-11-01T00:00:00Z")

	repo := gitwt.New(dir)
	if got := string(SharedVersionIn(repo)(legacy, key)); got != "v1" {
		t.Fatalf("shared version = %q, want v1", got)
	}
	if ChangedLaterIn(repo)(legacy, key) {
		t.Fatal("legacy was not changed after the key's file")
	}

	write(legacy, "v2") // an old-version PC edits the legacy copy
	commit("legacy v2", "2026-11-20T00:00:00Z")
	if got := string(SharedVersionIn(repo)(legacy, key)); got != "v1" {
		t.Fatalf("shared version after legacy edit = %q, want v1", got)
	}
	if !ChangedLaterIn(repo)(legacy, key) {
		t.Fatal("legacy should count as changed later")
	}
	if SharedVersionIn(repo)(legacy, "g-0123456789ab/memory/other.md") != nil {
		t.Fatal("a path with no history shares no version")
	}
}

func TestMigrateLayout_PruneRemovesOnlyImported(t *testing.T) {
	r, root := reconcileFixture(t)
	idx, key := fingerprintedProject(t, r, root, "-home-matta-app")
	writeMem(t, r.Mirror, "-home-matta-app", "MEMORY.md", "# App\n")
	extra := filepath.Join(r.Mirror, "-home-matta-app", "memory", "archive")
	if err := os.MkdirAll(extra, 0700); err != nil {
		t.Fatal(err)
	}

	rep, err := MigrateLayout(r, idx, MigrateOptions{State: NewMigrationState(), Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if !memExists(t, r.Mirror, key, "MEMORY.md") {
		t.Fatalf("file not imported before prune")
	}
	if memExists(t, r.Mirror, "-home-matta-app", "MEMORY.md") || len(rep.PrunedLegacy) != 1 {
		t.Errorf("imported legacy file not pruned; rep %#v", rep)
	}
	if _, err := os.Stat(extra); err != nil {
		t.Errorf("unmigrated subdir must survive pruning: %v", err)
	}
}

// After migration, a legacy dir that reappears (pushed by a PC still on the old
// version) must not be paired with the local project and written to — that
// kept it alive as a second, diverging copy.
func TestReconcile_DoesNotWriteLegacyDirOfFingerprintedProject(t *testing.T) {
	r, root := reconcileFixture(t)
	idx, key := fingerprintedProject(t, r, root, "-home-matta-app")
	writeMem(t, r.Claude, "-home-matta-app", "MEMORY.md", "# App\n\n## Local\nx\n")
	writeMem(t, r.Claude, "-home-matta-app", "feedback_a.md", "a")
	writeMem(t, r.Mirror, "-home-matta-app", "MEMORY.md", "# App\n\n## OldPC\ny\n")

	if _, err := Reconcile(r, idx, NewManifest()); err != nil {
		t.Fatal(err)
	}
	if memExists(t, r.Mirror, "-home-matta-app", "feedback_a.md") {
		t.Errorf("Reconcile wrote local files into the legacy dir")
	}
	if s := readMem(t, r.Mirror, "-home-matta-app", "MEMORY.md"); contains(s, "## Local") {
		t.Errorf("Reconcile merged into the legacy dir: %q", s)
	}
	if !memExists(t, r.Mirror, key, "feedback_a.md") {
		t.Errorf("local file missing under the fingerprint key")
	}
}

// Two checkouts of one repo on this machine (e.g. worktrees) share a key. An
// edit in one must reach the other, and a reconcile must leave them identical
// and stable — not flip the mirror between them and pile up backups.
func TestReconcile_SiblingCheckoutsConverge(t *testing.T) {
	r, root := reconcileFixture(t)
	repoA := filepath.Join(root, "a")
	repoB := filepath.Join(root, "b")
	makeRepo(t, repoA, "git@github.com:acme/app.git")
	makeRepo(t, repoB, "git@github.com:acme/app.git")
	makeClaudeProject(t, r.Claude, "-wt-a", repoA)
	makeClaudeProject(t, r.Claude, "-wt-b", repoB)
	writeMem(t, r.Claude, "-wt-a", "feedback_x.md", "new")
	writeMem(t, r.Claude, "-wt-b", "feedback_x.md", "old")
	idx, err := project.Build(r.Claude, nil)
	if err != nil {
		t.Fatal(err)
	}
	ea, _ := idx.Get("-wt-a")
	key := ea.Key
	writeMem(t, r.Mirror, key, "feedback_x.md", "old")

	if _, err := Reconcile(r, idx, NewManifest()); err != nil {
		t.Fatal(err)
	}
	a := readMem(t, r.Claude, "-wt-a", "feedback_x.md")
	b := readMem(t, r.Claude, "-wt-b", "feedback_x.md")
	m := readMem(t, r.Mirror, key, "feedback_x.md")
	if a != b || b != m {
		t.Fatalf("checkouts did not converge: a=%q b=%q mirror=%q", a, b, m)
	}

	// Stable: a second reconcile changes nothing.
	rep, err := Reconcile(r, idx, NewManifest())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(rep.CopiedToMirror) + len(rep.CopiedToClaude) + len(rep.BackedUp) + len(rep.Merged); n != 0 {
		t.Errorf("second reconcile should be a no-op; got %#v", rep)
	}

	// Incremental path: an edit in A fans out to B via CopyToMirror.
	writeMem(t, r.Claude, "-wt-a", "feedback_x.md", "newer")
	if err := CopyToMirror(r, idx, "-wt-a", "feedback_x.md"); err != nil {
		t.Fatal(err)
	}
	if got := readMem(t, r.Claude, "-wt-b", "feedback_x.md"); got != "newer" {
		t.Errorf("edit did not fan out to sibling checkout: %q", got)
	}

	// A delete in A (recorded in the manifest) removes it everywhere instead of
	// being undone by B's copy.
	manifest, err := BuildFromClaudeTree(r.Claude)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.Claude, "-wt-a", "memory", "feedback_x.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(r, idx, manifest); err != nil {
		t.Fatal(err)
	}
	if memExists(t, r.Claude, "-wt-b", "feedback_x.md") || memExists(t, r.Mirror, key, "feedback_x.md") {
		t.Errorf("delete in one checkout was undone by its sibling")
	}
}

// The first write in a project opened here for the first time must not
// overwrite memories another PC already synced under its key: the incremental
// path defers to Reconcile, which merges.
func TestCopyToMirror_NewProjectDefersToReconcile(t *testing.T) {
	r, root := reconcileFixture(t)
	idx, key := fingerprintedProject(t, r, root, "-p")
	writeMem(t, r.Mirror, key, "MEMORY.md", "# App\n\n## FromOtherPC\nremote\n")

	// A project the index doesn't know yet (opened mid-session).
	repo := filepath.Join(root, "repo-new")
	makeRepo(t, repo, "git@github.com:acme/app.git")
	makeClaudeProject(t, r.Claude, "-new", repo)
	writeMem(t, r.Claude, "-new", "MEMORY.md", "# App\n\n## Local\nlocal\n")

	if err := CopyToMirror(r, idx, "-new", "MEMORY.md"); !errors.Is(err, ErrNeedsReconcile) {
		t.Fatalf("CopyToMirror on unindexed project = %v; want ErrNeedsReconcile", err)
	}
	if s := readMem(t, r.Mirror, key, "MEMORY.md"); contains(s, "## Local") || !contains(s, "## FromOtherPC") {
		t.Fatalf("mirror changed before reconcile: %q", s)
	}

	// What the debounced flush then does: re-index and reconcile.
	idx, err := project.Build(r.Claude, idx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(r, idx, NewManifest()); err != nil {
		t.Fatal(err)
	}
	if s := readMem(t, r.Mirror, key, "MEMORY.md"); !contains(s, "## Local") || !contains(s, "## FromOtherPC") {
		t.Errorf("reconcile lost a side: %q", s)
	}
}

// siblingPair sets up two checkouts of one repo (worktrees) sharing a key, with
// the mirror and both checkouts holding base for name.
func siblingPair(t *testing.T, name, base string) (Roots, *project.Index, string) {
	t.Helper()
	r, root := reconcileFixture(t)
	for _, lh := range []string{"-wt-a", "-wt-b"} {
		repo := filepath.Join(root, lh)
		makeRepo(t, repo, "git@github.com:acme/app.git")
		makeClaudeProject(t, r.Claude, lh, repo)
		writeMem(t, r.Claude, lh, name, base)
	}
	idx, err := project.Build(r.Claude, nil)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := idx.Get("-wt-a")
	writeMem(t, r.Mirror, e.Key, name, base)
	return r, idx, e.Key
}

// Both checkouts edited before the watcher handled either: the first event
// must not overwrite the other checkout's edit.
func TestCopyToMirror_DivergentSiblingIsNotOverwritten(t *testing.T) {
	r, idx, key := siblingPair(t, "MEMORY.md", "# App\n")
	writeMem(t, r.Claude, "-wt-a", "MEMORY.md", "# App\n\n## A\na\n")
	writeMem(t, r.Claude, "-wt-b", "MEMORY.md", "# App\n\n## B\nb\n")

	if err := CopyToMirror(r, idx, "-wt-a", "MEMORY.md"); !errors.Is(err, ErrNeedsReconcile) {
		t.Fatalf("CopyToMirror with divergent sibling = %v; want ErrNeedsReconcile", err)
	}
	if s := readMem(t, r.Claude, "-wt-b", "MEMORY.md"); !contains(s, "## B") {
		t.Fatalf("sibling edit overwritten: %q", s)
	}

	manifest, err := BuildFromClaudeTree(r.Claude)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(r, idx, manifest); err != nil {
		t.Fatal(err)
	}
	for _, where := range []string{
		readMem(t, r.Claude, "-wt-a", "MEMORY.md"),
		readMem(t, r.Claude, "-wt-b", "MEMORY.md"),
		readMem(t, r.Mirror, key, "MEMORY.md"),
	} {
		if !contains(where, "## A") || !contains(where, "## B") {
			t.Errorf("reconcile did not keep both edits: %q", where)
		}
	}
}

// A delete in one checkout while the other has edited the file must not
// discard the edit: the edit outranks the delete.
func TestRemoveFromMirror_EditedSiblingOutranksDelete(t *testing.T) {
	r, idx, key := siblingPair(t, "feedback_x.md", "base")
	manifest, err := BuildFromClaudeTree(r.Claude)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(r.Claude, "-wt-a", "memory", "feedback_x.md")); err != nil {
		t.Fatal(err)
	}
	writeMem(t, r.Claude, "-wt-b", "feedback_x.md", "edited")

	if err := RemoveFromMirror(r, idx, "-wt-a", "feedback_x.md"); !errors.Is(err, ErrNeedsReconcile) {
		t.Fatalf("RemoveFromMirror with edited sibling = %v; want ErrNeedsReconcile", err)
	}
	if got := readMem(t, r.Claude, "-wt-b", "feedback_x.md"); got != "edited" {
		t.Fatalf("sibling edit discarded: %q", got)
	}
	if _, err := Reconcile(r, idx, manifest); err != nil {
		t.Fatal(err)
	}
	for _, got := range []string{
		readMem(t, r.Claude, "-wt-a", "feedback_x.md"),
		readMem(t, r.Mirror, key, "feedback_x.md"),
	} {
		if got != "edited" {
			t.Errorf("edit should be restored everywhere; got %q", got)
		}
	}
}

// A mirror memory dir that can't be read must fail the pass, not be treated as
// empty (which would let local content overwrite unread mirror files).
func TestReconcile_MirrorReadErrorPropagates(t *testing.T) {
	r, root := reconcileFixture(t)
	idx, key := fingerprintedProject(t, r, root, "-p")
	writeMem(t, r.Claude, "-p", "MEMORY.md", "# local\n")
	// A file where the memory dir should be: ReadDir fails with something other
	// than not-exist on every platform.
	if err := os.MkdirAll(filepath.Join(r.Mirror, key), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.Mirror, key, "memory"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(r, idx, NewManifest()); err == nil {
		t.Errorf("expected an error for an unreadable mirror dir")
	}
}

// RemoveFromMirror on an event for a file that is still present (atomic
// replace) must not delete anything.
func TestRemoveFromMirror_FileStillPresentIsCopied(t *testing.T) {
	r, root := reconcileFixture(t)
	idx, key := fingerprintedProject(t, r, root, "-p")
	writeMem(t, r.Claude, "-p", "MEMORY.md", "# x\n")
	if err := RemoveFromMirror(r, idx, "-p", "MEMORY.md"); err != nil {
		t.Fatal(err)
	}
	if !memExists(t, r.Mirror, key, "MEMORY.md") {
		t.Errorf("present file should have been copied, not removed")
	}
}
