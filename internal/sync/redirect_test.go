package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MarimerLLC/claude-utils/internal/project"
)

const testKey = "g-0123456789ab"

func redirectOf(t *testing.T, mirror, legacy string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(mirror, legacy, RedirectFileName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func TestMigrateLayout_PruneLeavesRedirect(t *testing.T) {
	r, root := reconcileFixture(t)
	idx, key := fingerprintedProject(t, r, root, "-home-matta-app")
	writeMem(t, r.Mirror, "-home-matta-app", "note.md", "x\n")

	rep, err := MigrateLayout(r, idx, MigrateOptions{State: NewMigrationState(), Prune: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := redirectOf(t, r.Mirror, "-home-matta-app"); got != key {
		t.Fatalf("redirect = %q, want %q", got, key)
	}
	if memExists(t, r.Mirror, "-home-matta-app", "note.md") || len(rep.Redirected) != 1 {
		t.Fatalf("legacy file not pruned or redirect not reported: %#v", rep)
	}
	// Idempotent: a second pass neither rewrites nor re-reports it.
	rep, err = MigrateLayout(r, idx, MigrateOptions{State: NewMigrationState(), Prune: true})
	if err != nil || len(rep.Redirected) != 0 {
		t.Fatalf("second pass: err=%v rep=%#v", err, rep)
	}
}

// Legacy dirs pruned by v0.4.0/v0.4.1 left no redirect; a pruning PC backfills
// one when history shows the dir existed.
func TestMigrateLayout_BackfillsRedirectForAlreadyPrunedDir(t *testing.T) {
	r, root := reconcileFixture(t)
	idx, key := fingerprintedProject(t, r, root, "-home-matta-app")
	had := func(lh string) bool { return lh == "-home-matta-app" }

	if _, err := MigrateLayout(r, idx, MigrateOptions{Prune: true, HadLegacy: had}); err != nil {
		t.Fatal(err)
	}
	if got := redirectOf(t, r.Mirror, "-home-matta-app"); got != key {
		t.Fatalf("backfilled redirect = %q, want %q", got, key)
	}

	// Without pruneLegacyDirs nothing is written.
	r2, root2 := reconcileFixture(t)
	idx2, _ := fingerprintedProject(t, r2, root2, "-home-matta-app")
	if _, err := MigrateLayout(r2, idx2, MigrateOptions{HadLegacy: had}); err != nil {
		t.Fatal(err)
	}
	if got := redirectOf(t, r2.Mirror, "-home-matta-app"); got != "" {
		t.Fatalf("redirect written without Prune: %q", got)
	}
}

// The first redirect wins: a PC that fingerprints the same path differently
// must not flip it under the PCs already following it.
func TestWriteRedirect_KeepsExisting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "-p")
	if err := writeRedirect(dir, testKey); err != nil {
		t.Fatal(err)
	}
	if err := writeRedirect(dir, "r-ffffffffffff"); err != nil {
		t.Fatal(err)
	}
	if got := redirectOf(t, filepath.Dir(dir), "-p"); got != testKey {
		t.Fatalf("redirect = %q, want the first %q", got, testKey)
	}
}

func TestFollowRedirects_MovesOnlyPathKeyedProjects(t *testing.T) {
	r, _ := reconcileFixture(t)
	idx := project.NewIndex()
	idx.Set("-a", project.Entry{Key: "-a", Method: project.MethodPath})
	idx.Set("-b", project.Entry{Key: "g-bbbbbbbbbbbb", Method: project.MethodRemote})
	idx.Set("-c", project.Entry{Key: "-c", Method: project.MethodPath})
	for _, lh := range []string{"-a", "-b"} {
		if err := writeRedirect(filepath.Join(r.Mirror, lh), testKey); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeFile(filepath.Join(r.Mirror, "-c", RedirectFileName), []byte("not a key\n")); err != nil {
		t.Fatal(err)
	}

	moved := FollowRedirects(r, idx)
	if len(moved) != 1 || moved[0] != "-a" {
		t.Fatalf("moved = %v, want [-a]", moved)
	}
	if e, _ := idx.Get("-a"); e.Key != testKey || e.Method != project.MethodMoved {
		t.Errorf("-a = %+v", e)
	}
	if e, _ := idx.Get("-b"); e.Key != "g-bbbbbbbbbbbb" {
		t.Errorf("a fingerprinted project must not follow a redirect: %+v", e)
	}
	if e, _ := idx.Get("-c"); e.Key != "-c" {
		t.Errorf("an unreadable redirect must be ignored: %+v", e)
	}
	// With the project moved, the legacy dir's deletions target nothing local.
	writeMem(t, r.Claude, "-a", "note.md", "x\n")
	if n := removeFromClaude(r, idx, "-a", "note.md"); n != 0 || !memExists(t, r.Claude, "-a", "note.md") {
		t.Fatalf("legacy deletion reached the moved project (targets=%d)", n)
	}
}

// AdoptRekeyed: a stale local copy (a version the key already held) gives way to
// the key's newer one instead of overwriting it.
func TestAdoptRekeyed_StaleLocalTakesKeyCopy(t *testing.T) {
	r, _ := reconcileFixture(t)
	writeMem(t, r.Claude, "-p", "note.md", "old\n")
	writeMem(t, r.Mirror, testKey, "note.md", "new\n")
	before := map[string]project.Entry{"-p": {Key: "-p", Method: project.MethodPath}}
	idx := project.NewIndex()
	idx.Set("-p", project.Entry{Key: testKey, Method: project.MethodRemote})
	prior := func(rel string, content []byte) bool {
		return rel == testKey+"/memory/note.md" && string(content) == "old\n"
	}

	adopted, rep := AdoptRekeyed(r, before, idx, MigrateOptions{Prior: prior})
	if len(adopted) != 1 || len(rep.Errors) != 0 {
		t.Fatalf("adopted=%v errors=%v", adopted, rep.Errors)
	}
	if got := readMem(t, r.Claude, "-p", "note.md"); got != "new\n" {
		t.Errorf("local = %q, want the key's newer copy", got)
	}
	if got := readMem(t, r.Mirror, testKey, "note.md"); got != "new\n" {
		t.Errorf("key = %q, must not be overwritten by the stale copy", got)
	}
}

// A real local edit is merged into the key against the shared version, and
// both sides end up with the merge.
func TestAdoptRekeyed_LocalEditMerges(t *testing.T) {
	r, _ := reconcileFixture(t)
	base := "a\nb\nc\n"
	writeMem(t, r.Claude, "-p", "note.md", "a LOCAL\nb\nc\n")
	writeMem(t, r.Mirror, testKey, "note.md", "a\nb\nc KEY\n")
	before := map[string]project.Entry{"-p": {Key: "-p", Method: project.MethodPath}}
	idx := project.NewIndex()
	idx.Set("-p", project.Entry{Key: testKey, Method: project.MethodMoved})

	_, rep := AdoptRekeyed(r, before, idx, MigrateOptions{
		Prior: func(string, []byte) bool { return false },
		Base:  func(string, string) []byte { return []byte(base) },
	})
	if len(rep.Errors) != 0 {
		t.Fatal(rep.Errors)
	}
	want := "a LOCAL\nb\nc KEY\n"
	if got := readMem(t, r.Mirror, testKey, "note.md"); got != want {
		t.Errorf("key = %q, want %q", got, want)
	}
	if got := readMem(t, r.Claude, "-p", "note.md"); got != want {
		t.Errorf("local = %q, want %q", got, want)
	}
}

// A file the key held and has since deleted is removed locally rather than
// resurrected; one the key never had is sent up.
func TestAdoptRekeyed_FollowsKeyDeletionAndUploadsNew(t *testing.T) {
	r, _ := reconcileFixture(t)
	writeMem(t, r.Claude, "-p", "gone.md", "was synced\n")
	writeMem(t, r.Claude, "-p", "fresh.md", "new here\n")
	before := map[string]project.Entry{"-p": {Key: "-p", Method: project.MethodPath}}
	idx := project.NewIndex()
	idx.Set("-p", project.Entry{Key: testKey, Method: project.MethodRemote})
	prior := func(rel string, content []byte) bool { return rel == testKey+"/memory/gone.md" }

	if _, rep := AdoptRekeyed(r, before, idx, MigrateOptions{Prior: prior}); len(rep.Errors) != 0 {
		t.Fatal(rep.Errors)
	}
	if memExists(t, r.Claude, "-p", "gone.md") {
		t.Error("a file the key deleted must not survive locally")
	}
	if got := readMem(t, r.Mirror, testKey, "fresh.md"); got != "new here\n" {
		t.Errorf("new local file not sent to the key: %q", got)
	}
}

func TestAdoptRekeyed_UnchangedKeysUntouched(t *testing.T) {
	r, _ := reconcileFixture(t)
	writeMem(t, r.Claude, "-p", "note.md", "local\n")
	writeMem(t, r.Mirror, testKey, "note.md", "key\n")
	before := map[string]project.Entry{"-p": {Key: testKey, Method: project.MethodRemote}}
	idx := project.NewIndex()
	idx.Set("-p", project.Entry{Key: testKey, Method: project.MethodRemote})
	idx.Set("-new", project.Entry{Key: "g-nnnnnnnnnnnn", Method: project.MethodRemote}) // not in before

	if adopted, _ := AdoptRekeyed(r, before, idx, MigrateOptions{}); len(adopted) != 0 {
		t.Fatalf("adopted %v; only key changes should be adopted", adopted)
	}
	if got := readMem(t, r.Claude, "-p", "note.md"); got != "local\n" {
		t.Errorf("local changed: %q", got)
	}
}

// A legacy dir holding only a redirect is not a project to materialize.
func TestReconcile_SkipsRedirectOnlyLegacyDir(t *testing.T) {
	r, _ := reconcileFixture(t)
	if err := writeRedirect(filepath.Join(r.Mirror, "-gone-app"), testKey); err != nil {
		t.Fatal(err)
	}
	idx := project.NewIndex()
	if _, err := Reconcile(r, idx, NewManifest()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.Claude, "-gone-app")); !os.IsNotExist(err) {
		t.Errorf("redirect-only legacy dir was materialized: %v", err)
	}
	if _, ok := idx.Get("-gone-app"); ok {
		t.Error("redirect-only legacy dir was indexed")
	}
}

// A PC upgrading to decoding: a materialized project folder (no transcript)
// decodes to a checkout whose key other PCs already fill with newer versions.
// The loop's index rebuild must adopt it, not let the stale materialized copy
// win the next reconcile.
func TestRebuildIndex_DecodedProjectAdoptsNewerKeyCopies(t *testing.T) {
	_, a, _ := conflictFixture(t)
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(base, "src", "the-app")
	makeRepo(t, checkout, "git@github.com:acme/the-app.git")
	lh := project.EncodePath(checkout)
	key, method := project.Fingerprint(checkout, lh)
	if method != project.MethodRemote {
		t.Fatalf("setup: %s keyed by %s", checkout, method)
	}

	// Sync repo history: the key held v1, then another PC moved it to v2.
	kmem := "projects/" + key + "/memory/"
	commitAt(t, a, kmem+"note.md", "v1\n", false)
	commitAt(t, a, kmem+"note.md", "v2 newer\n", false)

	claude := t.TempDir()
	writeMem(t, claude, lh, "note.md", "v1\n") // materialized long ago, never opened
	roots := Roots{Claude: claude, Mirror: filepath.Join(a.Dir, "projects")}
	l := testLoop(a)
	l.idx.Set(lh, project.Entry{Key: lh, Method: project.MethodPath}) // as v0.4.1 saved it

	l.rebuildIndex(a, roots)
	if e, _ := l.idx.Get(lh); e.Key != key {
		t.Fatalf("entry = %+v; want decoded to %s", e, key)
	}
	if _, err := Reconcile(roots, l.idx, NewManifest()); err != nil {
		t.Fatal(err)
	}
	if got := readAt(t, a, kmem+"note.md"); got != "v2 newer\n" {
		t.Fatalf("key note = %q; the stale materialized copy overwrote it", got)
	}
	if got := readMem(t, claude, lh, "note.md"); got != "v2 newer\n" {
		t.Fatalf("local note = %q; want the key's version", got)
	}
}

// End to end over a real remote: PC A fingerprints a project and prunes its
// legacy dir; PC B, which keys the same folder by path and holds stale copies,
// syncs. B must keep the project's memories (now the key's newer versions),
// move onto the key, never write the legacy dir back, and stay that way on the
// next sync and reconcile.
func TestPrunedElsewhere_PathKeyedPCFollowsAndStays(t *testing.T) {
	_, a, b := conflictFixture(t)
	const legacy = "-home-me-app"
	lmem := "projects/" + legacy + "/memory/"
	kmem := "projects/" + testKey + "/memory/"

	// Both PCs have the legacy dir; B has local copies of it.
	writeAt(t, a, lmem+"note.md", "v1\n")
	writeAt(t, a, lmem+"MEMORY.md", "# App\n\n## Notes\n- [note](note.md)\n")
	commitAll(t, a, "legacy")
	mustGit(t, a, "push", "-q", "origin", "main")
	mustPull(t, b)
	bClaude := t.TempDir()
	writeMem(t, bClaude, legacy, "note.md", "v1\n")
	writeMem(t, bClaude, legacy, "MEMORY.md", "# App\n\n## Notes\n- [note](note.md)\n")
	bRoots := Roots{Claude: bClaude, Mirror: filepath.Join(b.Dir, "projects")}
	lb := testLoop(b)
	lb.idx.Set(legacy, project.Entry{Key: legacy, Method: project.MethodPath})

	// A imports into the key (not pruning yet) and the key's copy moves on.
	// B syncs that first, so when the prune arrives the key's newer note is
	// already in B's sync repo and no longer part of the pull: only adopting the
	// re-keyed project stops B's stale v1 from overwriting it.
	aRoots := Roots{Claude: t.TempDir(), Mirror: filepath.Join(a.Dir, "projects")}
	aIdx := project.NewIndex()
	aIdx.Set(legacy, project.Entry{Key: testKey, Method: project.MethodRemote})
	if _, err := MigrateLayout(aRoots, aIdx, MigrateOptions{State: NewMigrationState(), Prior: PriorVersionIn(a)}); err != nil {
		t.Fatal(err)
	}
	commitAll(t, a, "import")
	commitAt(t, a, kmem+"note.md", "v2 newer\n", true)
	if err := lb.syncOnce(b, bRoots, true); err != nil {
		t.Fatalf("B first sync: %v", err)
	}
	if got := readMem(t, bClaude, legacy, "note.md"); got != "v1\n" {
		t.Fatalf("setup: B local note = %q before the prune", got)
	}

	// Now A prunes.
	mustPull(t, a)
	if _, err := MigrateLayout(aRoots, aIdx, MigrateOptions{State: NewMigrationState(), Prior: PriorVersionIn(a), Prune: true}); err != nil {
		t.Fatal(err)
	}
	commitAll(t, a, "prune")
	mustGit(t, a, "push", "-q", "origin", "main")

	if err := lb.syncOnce(b, bRoots, true); err != nil {
		t.Fatalf("B sync: %v", err)
	}
	if e, _ := lb.idx.Get(legacy); e.Key != testKey || e.Method != project.MethodMoved {
		t.Fatalf("B entry = %+v; want moved to %s", e, testKey)
	}
	if got := readMem(t, bClaude, legacy, "note.md"); got != "v2 newer\n" {
		t.Fatalf("B local note = %q; want the key's newer version", got)
	}
	if !memExists(t, bClaude, legacy, "MEMORY.md") {
		t.Fatal("B lost its MEMORY.md")
	}

	// Stays resolved: another sync and a pull-tick reconcile change nothing,
	// and nothing is ever written back into the legacy dir.
	for i := 0; i < 2; i++ {
		if _, err := Reconcile(bRoots, lb.idx, NewManifest()); err != nil {
			t.Fatal(err)
		}
		if err := lb.syncOnce(b, bRoots, true); err != nil {
			t.Fatalf("B sync %d: %v", i+2, err)
		}
	}
	if _, err := os.Stat(filepath.Join(b.Dir, filepath.FromSlash(lmem))); !os.IsNotExist(err) {
		t.Errorf("B recreated the pruned legacy dir: %v", err)
	}
	mustPull(t, a)
	if _, err := os.Stat(filepath.Join(a.Dir, filepath.FromSlash(lmem))); !os.IsNotExist(err) {
		t.Errorf("the legacy dir came back on A: %v", err)
	}
	if got := readAt(t, a, kmem+"note.md"); got != "v2 newer\n" {
		t.Errorf("key note on A = %q; B's stale copy must not have overwritten it", got)
	}
}
