package sync

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/config"
	"github.com/MarimerLLC/claude-utils/internal/gitwt"
	"github.com/MarimerLLC/claude-utils/internal/project"
)

const notePath = "projects/k/memory/note.md"

// conflictFixture is idleFixture with the sync repo's real .gitignore (so
// backups and .state stay local) and core.autocrlf off (so file contents
// compare byte for byte on Windows). Both clones start in sync.
func conflictFixture(t *testing.T) (origin string, a, b *gitwt.Repo) {
	t.Helper()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	mustGit(t, gitwt.New(root), "init", "--bare", "--initial-branch=main", origin)

	a = cloneFor(t, origin, filepath.Join(root, "a"))
	mustGit(t, a, "config", "core.autocrlf", "false")
	writeAt(t, a, ".gitignore", "config.json\ndaemon.pid\n.state/\n*.tmp\n*.tmp.*\n*.from-remote-*\n")
	commitFile(t, a, "seed.md", "seed")
	mustGit(t, a, "push", "--set-upstream", "origin", "main")

	b = cloneFor(t, origin, filepath.Join(root, "b"))
	mustGit(t, b, "config", "core.autocrlf", "false")
	return origin, a, b
}

func writeAt(t *testing.T, r *gitwt.Repo, rel, content string) {
	t.Helper()
	p := filepath.Join(r.Dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readAt(t *testing.T, r *gitwt.Repo, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(r.Dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func commitAll(t *testing.T, r *gitwt.Repo, msg string) {
	t.Helper()
	mustGit(t, r, "add", "-A")
	mustGit(t, r, "commit", "-q", "-m", msg)
}

// commitAt writes, commits and (if push) pushes one file.
func commitAt(t *testing.T, r *gitwt.Repo, rel, content string, push bool) {
	t.Helper()
	writeAt(t, r, rel, content)
	commitAll(t, r, "edit "+rel)
	if push {
		mustGit(t, r, "push", "-q", "origin", "main")
	}
}

func backupsOf(t *testing.T, r *gitwt.Repo, rel string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(r.Dir, filepath.FromSlash(rel)) + ".from-remote-*")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func revParse(t *testing.T, r *gitwt.Repo, rev string) string {
	t.Helper()
	out, err := r.Run("rev-parse", rev)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

func testLoop(r *gitwt.Repo) *Loop {
	return &Loop{Branch: "main", Hostname: "test", Cfg: config.Config{SyncDir: r.Dir}, idx: project.NewIndex()}
}

// mustPull runs the loop's pull (merge + conflict resolution + commit) and
// checks the work-tree ends clean, with no merge left in progress.
func mustPull(t *testing.T, r *gitwt.Repo) {
	t.Helper()
	if err := testLoop(r).pull(r); err != nil {
		t.Fatalf("pull: %v", err)
	}
	if r.MergeInProgress() {
		t.Fatal("merge still in progress after pull")
	}
	if out, _ := r.Run("ls-files", "-u"); out != "" {
		t.Fatalf("unmerged paths remain:\n%s", out)
	}
}

func assertNoMarkers(t *testing.T, content string) {
	t.Helper()
	for _, m := range []string{"<<<<<<<", ">>>>>>>"} {
		if strings.Contains(content, m) {
			t.Fatalf("conflict marker %q left in:\n%s", m, content)
		}
	}
}

func note(modified, body string) string {
	fm := "---\nname: note\ndescription: a note\nmetadata:\n  type: project\n"
	if modified != "" {
		fm += "  modified: " + modified + "\n"
	}
	return fm + "---\n\n" + body
}

const (
	t0 = "2026-11-20T10:00:00Z"
	t1 = "2026-11-21T10:00:00Z"
	t2 = "2026-11-22T10:00:00Z"
)

// 1. Both PCs edit the same line; origin's copy is newer, so it wins and this
// PC's version is kept as a backup. After pushing, both clones match.
func TestResolve_NewerRemoteWins(t *testing.T) {
	_, a, b := conflictFixture(t)
	commitAt(t, a, notePath, note(t0, "alpha\nbeta\ngamma\n"), true)
	mustPull(t, b)

	remote := note(t2, "alpha\nbeta-A\ngamma\n")
	local := note(t1, "alpha\nbeta-B\ngamma\n")
	commitAt(t, a, notePath, remote, true)
	commitAt(t, b, notePath, local, false)

	mustPull(t, b)
	if got := readAt(t, b, notePath); got != remote {
		t.Fatalf("newer remote should win:\n got %q\nwant %q", got, remote)
	}
	bk := backupsOf(t, b, notePath)
	if len(bk) != 1 {
		t.Fatalf("want one backup, got %v", bk)
	}
	if got, _ := os.ReadFile(bk[0]); string(got) != local {
		t.Fatalf("backup should hold this PC's original:\n got %q\nwant %q", got, local)
	}

	mustGit(t, b, "push", "-q", "origin", "main")
	mustPull(t, a)
	if revParse(t, a, "HEAD") != revParse(t, b, "HEAD") {
		t.Fatal("clones did not converge")
	}
	if readAt(t, a, notePath) != remote {
		t.Fatal("a's copy changed")
	}
}

// Ours newer: ours wins and origin's version is the backup.
func TestResolve_NewerLocalWins(t *testing.T) {
	_, a, b := conflictFixture(t)
	commitAt(t, a, notePath, note(t0, "alpha\nbeta\ngamma\n"), true)
	mustPull(t, b)

	remote := note(t1, "alpha\nbeta-A\ngamma\n")
	local := note(t2, "alpha\nbeta-B\ngamma\n")
	commitAt(t, a, notePath, remote, true)
	commitAt(t, b, notePath, local, false)

	mustPull(t, b)
	if got := readAt(t, b, notePath); got != local {
		t.Fatalf("newer local should win:\n got %q\nwant %q", got, local)
	}
	bk := backupsOf(t, b, notePath)
	if len(bk) != 1 {
		t.Fatalf("want one backup, got %v", bk)
	}
	if got, _ := os.ReadFile(bk[0]); string(got) != remote {
		t.Fatalf("backup should hold origin's version:\n got %q\nwant %q", got, remote)
	}
}

// 2. Only the timestamp collides, plus edits that don't overlap: a clean
// merge with the newer timestamp and no backup.
func TestResolve_TimestampOnlyConflictMergesClean(t *testing.T) {
	_, a, b := conflictFixture(t)
	body := "# Title\n\nintro\n\nl1\nl2\nl3\nl4\nl5\n"
	commitAt(t, a, notePath, note(t0, body), true)
	mustPull(t, b)

	commitAt(t, a, notePath, note(t2, strings.Replace(body, "l1", "l1-A", 1)), true)
	commitAt(t, b, notePath, note(t1, strings.Replace(body, "l5", "l5-B", 1)), false)

	mustPull(t, b)
	want := note(t2, strings.Replace(strings.Replace(body, "l1", "l1-A", 1), "l5", "l5-B", 1))
	if got := readAt(t, b, notePath); got != want {
		t.Fatalf("clean merge expected:\n got %q\nwant %q", got, want)
	}
	if bk := backupsOf(t, b, notePath); len(bk) != 0 {
		t.Fatalf("no backup expected, got %v", bk)
	}
}

// 3. No frontmatter: this PC wins and origin's version is the backup.
func TestResolve_NoTimestampLocalWins(t *testing.T) {
	_, a, b := conflictFixture(t)
	const rel = "projects/k/memory/plain.md"
	commitAt(t, a, rel, "a\nb\nc\n", true)
	mustPull(t, b)

	commitAt(t, a, rel, "a\nb-theirs\nc\n", true)
	commitAt(t, b, rel, "a\nb-ours\nc\n", false)

	mustPull(t, b)
	if got := readAt(t, b, rel); got != "a\nb-ours\nc\n" {
		t.Fatalf("this PC should win: %q", got)
	}
	bk := backupsOf(t, b, rel)
	if len(bk) != 1 {
		t.Fatalf("want one backup, got %v", bk)
	}
	if got, _ := os.ReadFile(bk[0]); string(got) != "a\nb-theirs\nc\n" {
		t.Fatalf("backup should hold origin's version: %q", got)
	}
	// Backups are git-ignored: they must not be committed.
	if out, _ := b.Run("ls-files", "--", "projects/k/memory/"); strings.Contains(out, "from-remote") {
		t.Fatalf("backup was staged:\n%s", out)
	}
}

// 4. Both PCs create the same file: the newer one wins.
func TestResolve_AddAdd(t *testing.T) {
	_, a, b := conflictFixture(t)
	const rel = "projects/k/memory/new.md"
	remote := note(t2, "remote text\nmore remote\n")
	local := note(t1, "local text\n")
	commitAt(t, a, rel, remote, true)
	commitAt(t, b, rel, local, false)

	mustPull(t, b)
	got := readAt(t, b, rel)
	assertNoMarkers(t, got)
	if got != remote {
		t.Fatalf("newer add should win:\n got %q\nwant %q", got, remote)
	}
	if bk := backupsOf(t, b, rel); len(bk) != 1 {
		t.Fatalf("want one backup, got %v", bk)
	}
}

// 5. Modified on one side, deleted on the other: the modified copy is kept,
// whichever side deleted it.
func TestResolve_ModifyDelete(t *testing.T) {
	for _, localDeletes := range []bool{false, true} {
		t.Run(fmt.Sprintf("localDeletes=%v", localDeletes), func(t *testing.T) {
			_, a, b := conflictFixture(t)
			commitAt(t, a, notePath, note(t0, "v1\n"), true)
			mustPull(t, b)

			edited := note(t1, "v2\n")
			deleter, editor := a, b
			if localDeletes {
				deleter, editor = b, a
			}
			if err := os.Remove(filepath.Join(deleter.Dir, filepath.FromSlash(notePath))); err != nil {
				t.Fatal(err)
			}
			commitAll(t, deleter, "delete")
			commitAt(t, editor, notePath, edited, false)
			mustGit(t, a, "push", "-q", "origin", "main") // a is origin's side

			mustPull(t, b)
			if got := readAt(t, b, notePath); got != edited {
				t.Fatalf("modified copy should be kept: %q", got)
			}
		})
	}
}

// 6. Many local commits against one conflicting remote commit resolve in a
// single merge, and the next push succeeds. (A rebase would replay, and could
// stop on, every one of the 50.)
func TestResolve_ManyLocalCommitsOneMerge(t *testing.T) {
	_, a, b := conflictFixture(t)
	commitAt(t, a, notePath, note(t0, "line\n"), true)
	mustPull(t, b)

	commitAt(t, a, notePath, note(t2, "remote\n"), true)
	for i := 0; i < 50; i++ {
		commitAt(t, b, notePath, note(t1, fmt.Sprintf("local %d\n", i)), false)
	}
	before := revParse(t, b, "HEAD")

	mustPull(t, b)
	out, err := b.Run("rev-list", "--merges", "--count", before+"..HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out) != "1" {
		t.Fatalf("want exactly one merge commit, got %s", out)
	}
	if got := readAt(t, b, notePath); got != note(t2, "remote\n") {
		t.Fatalf("newer remote should win: %q", got)
	}
	if err := b.Push("main"); err != nil {
		t.Fatalf("push after merge: %v", err)
	}
}

// 7. A MEMORY.md conflict is merged in-process even with no merge driver
// configured: both versions kept, no markers.
func TestResolve_MemoryIndexWithoutDriver(t *testing.T) {
	_, a, b := conflictFixture(t)
	const rel = "projects/k/memory/MEMORY.md"
	commitAt(t, a, rel, "## A\nx\n", true)
	mustPull(t, b)

	commitAt(t, a, rel, "## A\nx-theirs\n", true)
	commitAt(t, b, rel, "## A\nx-ours\n", false)

	mustPull(t, b)
	got := readAt(t, b, rel)
	assertNoMarkers(t, got)
	if !strings.Contains(got, "x-ours") || !strings.Contains(got, "x-theirs") {
		t.Fatalf("both versions should be kept: %q", got)
	}
	if bk := backupsOf(t, b, rel); len(bk) != 0 {
		t.Fatalf("MEMORY.md needs no backup, got %v", bk)
	}
}

// 8. A full flush ends a conflicting state with no error and origin == HEAD;
// failures are counted in health.json until a sync succeeds again.
func TestFlush_ResolvesConflictAndTracksHealth(t *testing.T) {
	origin, a, b := conflictFixture(t)
	commitAt(t, a, notePath, note(t0, "base\n"), true)
	mustPull(t, b)
	commitAt(t, a, notePath, note(t2, "remote\n"), true)
	writeAt(t, b, notePath, note(t1, "local\n")) // uncommitted: flush commits it

	l := testLoop(b)
	roots := Roots{Claude: t.TempDir(), Mirror: filepath.Join(b.Dir, "projects")}
	if err := l.flush(b, roots, true, nil); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if remote, _ := b.LsRemoteHead("main"); remote != revParse(t, b, "HEAD") {
		t.Fatalf("origin %s != HEAD %s after flush", remote, revParse(t, b, "HEAD"))
	}
	h, err := LoadHealth(b.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if h.ConsecutiveFailures != 0 || h.LastSuccess.IsZero() {
		t.Fatalf("health after success: %+v", h)
	}

	// Origin unreachable: failures accumulate, FirstFailure stays put.
	mustGit(t, b, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing.git"))
	if err := l.flush(b, roots, true, nil); err == nil {
		t.Fatal("flush against a missing origin should fail")
	}
	h1, _ := LoadHealth(b.Dir)
	if err := l.flush(b, roots, true, nil); err == nil {
		t.Fatal("flush against a missing origin should fail")
	}
	h2, _ := LoadHealth(b.Dir)
	if h1.ConsecutiveFailures != 1 || h2.ConsecutiveFailures != 2 {
		t.Fatalf("failures = %d then %d, want 1 then 2", h1.ConsecutiveFailures, h2.ConsecutiveFailures)
	}
	if h1.FirstFailure.IsZero() || !h2.FirstFailure.Equal(h1.FirstFailure) {
		t.Fatalf("FirstFailure moved: %v then %v", h1.FirstFailure, h2.FirstFailure)
	}
	if h2.LastError == "" || !h2.LastSuccess.Equal(h.LastSuccess) {
		t.Fatalf("health while failing: %+v", h2)
	}

	// Recovery resets the run.
	mustGit(t, b, "remote", "set-url", "origin", origin)
	if err := l.flush(b, roots, true, nil); err != nil {
		t.Fatalf("flush after recovery: %v", err)
	}
	h3, _ := LoadHealth(b.Dir)
	if h3.ConsecutiveFailures != 0 || !h3.FirstFailure.IsZero() || h3.LastError != "" {
		t.Fatalf("health after recovery: %+v", h3)
	}
}

func TestModifiedOf(t *testing.T) {
	cases := []struct {
		name, in, raw string
		ok            bool
	}{
		{"top level", "---\nname: x\nmodified: 2026-11-21T10:00:00Z\n---\nbody\n", "2026-11-21T10:00:00Z", true},
		{"under metadata", "---\nmetadata:\n  type: user\n  modified: 2026-11-21T10:00:00-06:00\n---\n", "2026-11-21T10:00:00-06:00", true},
		{"quoted", "---\nmodified: \"2026-11-21T10:00:00Z\"  \n---\n", "\"2026-11-21T10:00:00Z\"", true},
		{"crlf", "---\r\nmodified: 2026-11-21T10:00:00Z\r\n---\r\n", "2026-11-21T10:00:00Z", true},
		{"bom", "\ufeff---\nmodified: 2026-11-21\n---\n", "2026-11-21", true},
		{"nested elsewhere", "---\nother:\n  modified: 2026-11-21T10:00:00Z\n---\n", "", false},
		{"in body only", "---\nname: x\n---\nmodified: 2026-11-21T10:00:00Z\n", "", false},
		{"no frontmatter", "modified: 2026-11-21T10:00:00Z\n", "", false},
		{"unclosed", "---\nmodified: 2026-11-21T10:00:00Z\nbody\n", "", false},
		{"unparseable", "---\nmodified: yesterday\n---\n", "yesterday", false},
		{"trailing comment", "---\nmodified: 2026-11-21T10:00:00Z # moved\n---\n", "2026-11-21T10:00:00Z", true},
		{"quoted with comment", "---\nmodified: '2026-11-21T10:00:00Z'  # x\n---\n", "'2026-11-21T10:00:00Z'", true},
		{"hash inside value", "---\nmodified: 2026#11\n---\n", "2026#11", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw, _, ok := modifiedOf([]byte(c.in))
			if raw != c.raw || ok != c.ok {
				t.Errorf("modifiedOf = (%q, %v), want (%q, %v)", raw, ok, c.raw, c.ok)
			}
		})
	}
}

func TestSetModified(t *testing.T) {
	in := "---\nmetadata:\n  modified: 2026-11-20T10:00:00Z\n---\nmodified: keep me\n"
	want := "---\nmetadata:\n  modified: 2026-11-22T10:00:00Z\n---\nmodified: keep me\n"
	if got := string(setModified([]byte(in), "2026-11-22T10:00:00Z")); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	commented := "---\nmodified: 2026-11-20T10:00:00Z # moved\n---\n"
	if got := string(setModified([]byte(commented), "2026-11-22T10:00:00Z")); got != "---\nmodified: 2026-11-22T10:00:00Z # moved\n---\n" {
		t.Fatalf("comment should survive the rewrite: %q", got)
	}
	plain := "no frontmatter\n"
	if got := string(setModified([]byte(plain), "x")); got != plain {
		t.Fatalf("content without the key must be unchanged: %q", got)
	}
}

// Equal timestamps, or one side without one: this PC wins.
func TestResolveText_TiesGoToThisPC(t *testing.T) {
	base := []byte(note(t0, "a\nb\nc\n"))
	for name, theirsStamp := range map[string]string{"equal": t1, "theirs missing": ""} {
		t.Run(name, func(t *testing.T) {
			ours := []byte(note(t1, "a\nb-ours\nc\n"))
			theirs := []byte(note(theirsStamp, "a\nb-theirs\nc\n"))
			r := resolveText(base, ours, theirs)
			if !strings.Contains(string(r.content), "b-ours") || strings.Contains(string(r.content), "b-theirs") {
				t.Fatalf("this PC should win: %q", r.content)
			}
			if string(r.loser) != string(theirs) {
				t.Fatalf("loser should be theirs' original: %q", r.loser)
			}
		})
	}
}

// Binary content can't go through git merge-file (it exits 255): the winner
// is taken whole instead of aborting the merge, so the PC can't wedge.
func TestResolve_BinaryConflictTakesWholeFile(t *testing.T) {
	_, a, b := conflictFixture(t)
	const rel = "projects/k/memory/blob.bin"
	commitAt(t, a, rel, "a\x00base\n", true)
	mustPull(t, b)

	commitAt(t, a, rel, "a\x00theirs\n", true)
	commitAt(t, b, rel, "a\x00ours\n", false)

	mustPull(t, b)
	if got := readAt(t, b, rel); got != "a\x00ours\n" {
		t.Fatalf("this PC's whole file should win: %q", got)
	}
	bk := backupsOf(t, b, rel)
	if len(bk) != 1 {
		t.Fatalf("want one backup, got %v", bk)
	}
	if got, _ := os.ReadFile(bk[0]); string(got) != "a\x00theirs\n" {
		t.Fatalf("backup should hold origin's version: %q", got)
	}
	if err := b.Push("main"); err != nil {
		t.Fatalf("push after merge: %v", err)
	}
}

// A merge that is aborted (here: the commit is refused by a hook) must not
// leave backups behind; they are written only once the merge is committed.
func TestPull_AbortedMergeWritesNoBackup(t *testing.T) {
	_, a, b := conflictFixture(t)
	const rel = "projects/k/memory/plain.md"
	commitAt(t, a, rel, "a\nb\nc\n", true)
	mustPull(t, b)
	commitAt(t, a, rel, "a\nb-theirs\nc\n", true)
	commitAt(t, b, rel, "a\nb-ours\nc\n", false)

	out, err := b.Run("rev-parse", "--git-path", "hooks")
	if err != nil {
		t.Fatal(err)
	}
	hooks := strings.TrimSpace(out)
	if !filepath.IsAbs(hooks) {
		hooks = filepath.Join(b.Dir, hooks)
	}
	hook := filepath.Join(hooks, "pre-commit")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ { // retried, as the loop would every tick
		if err := testLoop(b).pull(b); err == nil {
			t.Fatal("pull should fail when the merge commit is refused")
		}
		if b.MergeInProgress() {
			t.Fatal("merge should be aborted")
		}
		if bk := backupsOf(t, b, rel); len(bk) != 0 {
			t.Fatalf("aborted merge left backups: %v", bk)
		}
	}

	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	mustPull(t, b)
	if bk := backupsOf(t, b, rel); len(bk) != 1 {
		t.Fatalf("want one backup after the merge commits, got %v", bk)
	}
}

// A rename/rename leaves the original path with only a base stage (deleted on
// both sides). Its name has glob characters: removing it must not drop a
// file the glob would also match (n1.md) from the merge.
func TestResolve_RenameRenameLiteralPath(t *testing.T) {
	_, a, b := conflictFixture(t)
	const dir = "projects/k/memory/"
	writeAt(t, a, dir+"n[12].md", "x\n")
	commitAt(t, a, dir+"n1.md", "one\n", true)
	mustPull(t, b)

	mustGit(t, a, "mv", dir+"n[12].md", dir+"ra.md")
	commitAll(t, a, "rename on a")
	mustGit(t, a, "push", "-q", "origin", "main")
	mustGit(t, b, "mv", dir+"n[12].md", dir+"rb.md")
	commitAll(t, b, "rename on b")

	mustPull(t, b)
	for _, name := range []string{"n1.md", "ra.md", "rb.md"} {
		if _, err := b.Run("cat-file", "-e", "HEAD:"+dir+name); err != nil {
			t.Errorf("%s missing from the merge: %v", name, err)
		}
	}
	if _, err := b.Run("cat-file", "-e", "HEAD:"+dir+"n[12].md"); err == nil {
		t.Error("n[12].md should be gone (renamed on both sides)")
	}
	if got := readAt(t, b, dir+"n1.md"); got != "one\n" {
		t.Errorf("n1.md changed: %q", got)
	}
	if err := b.Push("main"); err != nil {
		t.Fatalf("push after merge: %v", err)
	}
}

// A trailing YAML comment doesn't hide the timestamp: the newer side wins.
func TestResolveText_TimestampWithComment(t *testing.T) {
	base := []byte("---\nmodified: 2026-11-20T10:00:00Z\n---\na\nb\nc\n")
	ours := []byte("---\nmodified: 2026-11-21T10:00:00Z\n---\na\nb-ours\nc\n")
	theirs := []byte("---\nmodified: 2026-11-22T10:00:00Z # moved\n---\na\nb-theirs\nc\n")
	r := resolveText(base, ours, theirs)
	if string(r.content) != string(theirs) {
		t.Fatalf("newer (commented) side should win:\n got %q\nwant %q", r.content, theirs)
	}
	if string(r.loser) != string(ours) {
		t.Fatalf("loser should be ours: %q", r.loser)
	}
}

// A merge left half-done (and, here, impossible to abort because of a stale
// index.lock) must never be staged and committed with its conflict markers.
// The sync refuses until the reset works, then recovers.
func TestFlush_RefusesToStageMidMerge(t *testing.T) {
	_, a, b := conflictFixture(t)
	const rel = "projects/k/memory/plain.md"
	commitAt(t, a, rel, "a\nb\nc\n", true)
	mustPull(t, b)
	commitAt(t, a, rel, "a\nb-theirs\nc\n", true)
	commitAt(t, b, rel, "a\nb-ours\nc\n", false)
	if err := b.Pull("main"); err == nil || !b.MergeInProgress() {
		t.Fatalf("expected the raw pull to stop mid-merge (err=%v)", err)
	}
	before := revParse(t, b, "HEAD")

	out, err := b.Run("rev-parse", "--git-path", "index.lock")
	if err != nil {
		t.Fatal(err)
	}
	lock := strings.TrimSpace(out)
	if !filepath.IsAbs(lock) {
		lock = filepath.Join(b.Dir, lock)
	}
	if err := os.WriteFile(lock, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	l := testLoop(b)
	roots := Roots{Claude: t.TempDir(), Mirror: filepath.Join(b.Dir, "projects")}
	if err := l.flush(b, roots, true, nil); err == nil {
		t.Fatal("flush must fail while the merge can't be reset")
	}
	if got := revParse(t, b, "HEAD"); got != before {
		t.Fatalf("HEAD moved from %s to %s: something was committed mid-merge", before, got)
	}
	if h, _ := LoadHealth(b.Dir); h.ConsecutiveFailures != 1 {
		t.Fatalf("failure not recorded: %+v", h)
	}

	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
	if err := l.flush(b, roots, true, nil); err != nil {
		t.Fatalf("flush after the lock is gone: %v", err)
	}
	got := readAt(t, b, rel)
	assertNoMarkers(t, got)
	if got != "a\nb-ours\nc\n" {
		t.Fatalf("this PC should win after recovery: %q", got)
	}
	if remote, _ := b.LsRemoteHead("main"); remote != revParse(t, b, "HEAD") {
		t.Fatal("origin != HEAD after recovery")
	}
}

// A Reconcile failure earlier in the tick makes the tick a failure even when
// the git phase is idle, so status can't report ok while memories go unsynced.
func TestFlush_RecordsPriorError(t *testing.T) {
	_, _, b := conflictFixture(t)
	l := testLoop(b)
	roots := Roots{Claude: t.TempDir(), Mirror: filepath.Join(b.Dir, "projects")}
	if err := l.flush(b, roots, true, errors.New("reconcile: boom")); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("flush should return the prior error, got %v", err)
	}
	h, _ := LoadHealth(b.Dir)
	if h.ConsecutiveFailures != 1 || !strings.Contains(h.LastError, "boom") {
		t.Fatalf("prior error not recorded: %+v", h)
	}
	if err := l.flush(b, roots, true, nil); err != nil {
		t.Fatal(err)
	}
	if h, _ := LoadHealth(b.Dir); h.ConsecutiveFailures != 0 {
		t.Fatalf("clean tick should reset: %+v", h)
	}
}
