package merge

import (
	"strings"
	"testing"
)

func TestParse_NoHeadings(t *testing.T) {
	in := "# Title\nsome intro text\n"
	got := Parse(in)
	if len(got) != 1 {
		t.Fatalf("want 1 block, got %d", len(got))
	}
	if got[0].Key != PreambleKey {
		t.Errorf("expected preamble key, got %q", got[0].Key)
	}
	if got[0].Body != in {
		t.Errorf("preamble body should be entire input, got %q", got[0].Body)
	}
}

func TestParse_PreambleAndSections(t *testing.T) {
	in := "# RockBot\n\nIntro paragraph.\n\n## Deploy\nbody1\n\n## Build\nbody2\n"
	got := Parse(in)
	if len(got) != 3 {
		t.Fatalf("want 3 blocks, got %d: %#v", len(got), got)
	}
	if got[0].Key != PreambleKey {
		t.Errorf("first block should be preamble, got %q", got[0].Key)
	}
	if got[1].Key != "deploy" {
		t.Errorf("second block key=%q, want deploy", got[1].Key)
	}
	if got[2].Key != "build" {
		t.Errorf("third block key=%q, want build", got[2].Key)
	}
	if !strings.Contains(got[1].Heading, "## Deploy") {
		t.Errorf("heading missing: %q", got[1].Heading)
	}
}

func TestParse_DuplicateHeadingsGetDistinctKeys(t *testing.T) {
	in := "# T\n\n## Notes\nfirst\n\n## Notes\nsecond\n"
	got := Parse(in)
	if len(got) != 3 {
		t.Fatalf("want 3 blocks (preamble + 2 sections), got %d: %#v", len(got), got)
	}
	if got[1].Key == got[2].Key {
		t.Fatalf("repeated headings must get distinct keys; both are %q", got[1].Key)
	}
	if got[1].Key != "notes" {
		t.Errorf("first occurrence should keep the plain key, got %q", got[1].Key)
	}
	if !strings.Contains(got[1].Body, "first") || !strings.Contains(got[2].Body, "second") {
		t.Errorf("bodies mismatched: %q / %q", got[1].Body, got[2].Body)
	}
}

// A MEMORY.md that repeats an H2 must survive a merge intact. Before the
// occurrence-numbering fix, blocksByKey collapsed both sections into one map
// entry and the first one's body was dropped — even here, where all three
// inputs are byte-identical and the merge should be a no-op.
func TestMerge_DuplicateHeadingsPreserved(t *testing.T) {
	doc := "# T\n\n## Notes\nfirst\n\n## Notes\nsecond\n"

	got, conflict := Merge(doc, doc, doc)
	if conflict {
		t.Fatalf("identical inputs must not conflict; got=%q", got)
	}
	if n := strings.Count(got, "## Notes"); n != 2 {
		t.Errorf("expected both Notes headings preserved, got %d in %q", n, got)
	}
	for _, want := range []string{"first", "second"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost body %q: %q", want, got)
		}
	}
}

// Keys are lowercased so "## Deploy" on one PC matches "## deploy" on another.
// That must not make two case-variant headings in the *same* file collapse.
func TestMerge_CaseVariantHeadingsBothKept(t *testing.T) {
	doc := "# T\n\n## Notes\nupper\n\n## notes\nlower\n"

	got, conflict := Merge(doc, doc, doc)
	if conflict {
		t.Fatalf("identical inputs must not conflict; got=%q", got)
	}
	for _, want := range []string{"upper", "lower"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost body %q: %q", want, got)
		}
	}
}

// Each occurrence is merged against its own counterpart, so edits to different
// repeats of the same heading combine cleanly instead of fighting.
func TestMerge_DuplicateHeadingsMatchByOccurrence(t *testing.T) {
	base := "## Notes\nfirst\n\n## Notes\nsecond\n"
	ours := "## Notes\nfirst edited by us\n\n## Notes\nsecond\n"
	theirs := "## Notes\nfirst\n\n## Notes\nsecond edited by them\n"

	got, conflict := Merge(base, ours, theirs)
	if conflict {
		t.Fatalf("edits to different occurrences should merge cleanly; got=%q", got)
	}
	if !strings.Contains(got, "first edited by us") {
		t.Errorf("lost our edit to the first occurrence: %q", got)
	}
	if !strings.Contains(got, "second edited by them") {
		t.Errorf("lost their edit to the second occurrence: %q", got)
	}
}

// Deleting an earlier copy must not shift the later copies onto the wrong
// counterpart. Matching by position alone paired ours' surviving "second" with
// base's "first", so theirs' edit to "second" came back next to the stale
// original, with no conflict reported.
func TestMerge_DuplicateDeletedOnOneSideEditedOnOther(t *testing.T) {
	base := "## Notes\nfirst\n\n## Notes\nsecond\n"
	ours := "## Notes\nsecond\n"
	theirs := "## Notes\nfirst\n\n## Notes\nsecond edited by them\n"

	got, conflict := Merge(base, ours, theirs)
	want := "## Notes\nsecond edited by them\n"
	if conflict || got != want {
		t.Fatalf("got (%v) %q, want %q", conflict, got, want)
	}
}

func TestMerge_DuplicateDeletedOnOneSideUntouchedOnOther(t *testing.T) {
	base := "## Notes\nfirst\n\n## Notes\nsecond\n"
	ours := "## Notes\nsecond\n"

	got, conflict := Merge(base, ours, base)
	if conflict || got != ours {
		t.Fatalf("got (%v) %q, want %q", conflict, got, ours)
	}
}

// Inserting a new copy above an existing one must not pair the new copy with
// the old one. Matching by position alone reported a conflict between "new"
// and theirs' edit and kept a stale copy of "first" as well.
func TestMerge_DuplicateInsertedAboveEditedCopy(t *testing.T) {
	base := "## Notes\nfirst\n"
	ours := "## Notes\nnew\n\n## Notes\nfirst\n"
	theirs := "## Notes\nfirst edited by them\n"

	got, conflict := Merge(base, ours, theirs)
	want := "## Notes\nnew\n\n## Notes\nfirst edited by them\n"
	if conflict || got != want {
		t.Fatalf("got (%v) %q, want %q", conflict, got, want)
	}
}

// Reconcile merges with an empty base, so copies are matched between ours and
// theirs directly: a copy both sides have is kept once, extra copies are kept.
func TestMerge_DuplicatesWithEmptyBase(t *testing.T) {
	ours := "## Notes\nb\n"
	theirs := "## Notes\na\n\n## Notes\nb\n"

	got, conflict := Merge("", ours, theirs)
	if conflict {
		t.Fatalf("unexpected conflict: %q", got)
	}
	if strings.Count(got, "## Notes") != 2 || strings.Count(got, "b\n") != 1 || !strings.Contains(got, "a\n") {
		t.Fatalf("want one copy each of a and b, got %q", got)
	}
}

// Both sides adding the same new copy keeps it once, as for any other section.
func TestMerge_SameDuplicateAddedOnBothSides(t *testing.T) {
	base := "## Notes\na\n"
	both := "## Notes\na\n\n## Notes\nb\n"

	got, conflict := Merge(base, both, both)
	if conflict || got != both {
		t.Fatalf("got (%v) %q, want %q", conflict, got, both)
	}
}

// When both sides change the copies so no copy anchors the alignment, the
// overlap must keep both texts rather than lose either one.
func TestMerge_DuplicatesBothChangedKeepBoth(t *testing.T) {
	base := "## Notes\na\n\n## Notes\nb\n"
	ours := "## Notes\na edited\n\n## Notes\nb\n"
	theirs := "## Notes\nb edited\n"

	got, conflict := Merge(base, ours, theirs)
	if conflict {
		t.Fatalf("both changes should be kept without a conflict, got %q", got)
	}
	if strings.Contains(got, "<<<<<<<") {
		t.Errorf("unexpected conflict markers: %q", got)
	}
	for _, want := range []string{"a edited", "b edited"} {
		if !strings.Contains(got, want) {
			t.Errorf("lost %q: %q", want, got)
		}
	}
}

func TestMerge_BothAddDisjointSections(t *testing.T) {
	base := "# Project\n"
	ours := "# Project\n\n## Deploy\nuse helm.\n"
	theirs := "# Project\n\n## Build\nuse docker.\n"

	got, conflict := Merge(base, ours, theirs)
	if conflict {
		t.Fatalf("unexpected conflict; got=%q", got)
	}
	if !strings.Contains(got, "## Deploy") {
		t.Errorf("missing Deploy section: %q", got)
	}
	if !strings.Contains(got, "## Build") {
		t.Errorf("missing Build section: %q", got)
	}
	if !strings.Contains(got, "use helm.") || !strings.Contains(got, "use docker.") {
		t.Errorf("bodies missing: %q", got)
	}
}

func TestMerge_IdenticalAdditionDeduped(t *testing.T) {
	base := "# Project\n"
	same := "# Project\n\n## Notes\nshared content.\n"
	got, conflict := Merge(base, same, same)
	if conflict {
		t.Fatalf("unexpected conflict; got=%q", got)
	}
	count := strings.Count(got, "## Notes")
	if count != 1 {
		t.Errorf("expected exactly one Notes heading, got %d in %q", count, got)
	}
}

func TestMerge_ListUnion(t *testing.T) {
	base := "## Memories\n- [A](a.md) — alpha\n"
	ours := "## Memories\n- [A](a.md) — alpha\n- [B](b.md) — beta from PC1\n"
	theirs := "## Memories\n- [A](a.md) — alpha\n- [C](c.md) — gamma from PC2\n"

	got, conflict := Merge(base, ours, theirs)
	if conflict {
		t.Fatalf("list union should not conflict; got=%q", got)
	}
	for _, want := range []string{"a.md", "b.md", "c.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in merged: %q", want, got)
		}
	}
}

func TestMerge_ListDedupSameLink(t *testing.T) {
	base := "## Memories\n"
	ours := "## Memories\n- [A](a.md) — first description\n"
	theirs := "## Memories\n- [A](a.md) — second description\n"

	got, _ := Merge(base, ours, theirs)
	count := strings.Count(got, "(a.md)")
	if count != 1 {
		t.Errorf("expected one entry for a.md (deduped by link), got %d in %q", count, got)
	}
}

func TestMerge_DeleteVsUnchanged(t *testing.T) {
	base := "# P\n\n## Old\nbody\n\n## Keep\nstable\n"
	ours := "# P\n\n## Keep\nstable\n"
	theirs := "# P\n\n## Old\nbody\n\n## Keep\nstable\n"

	got, conflict := Merge(base, ours, theirs)
	if conflict {
		t.Fatalf("clean delete-vs-unchanged should not conflict; got=%q", got)
	}
	if strings.Contains(got, "## Old") {
		t.Errorf("Old should have been deleted: %q", got)
	}
	if !strings.Contains(got, "## Keep") {
		t.Errorf("Keep should remain: %q", got)
	}
}

func TestMerge_DeleteVsModify(t *testing.T) {
	base := "# P\n\n## Old\nbody\n"
	ours := "# P\n"
	theirs := "# P\n\n## Old\nupdated body\n"

	got, conflict := Merge(base, ours, theirs)
	if conflict {
		t.Fatalf("delete-vs-modify keeps modified, no conflict expected: %v / %q", conflict, got)
	}
	if !strings.Contains(got, "updated body") {
		t.Errorf("expected modified body to win over delete: %q", got)
	}
}

func TestMerge_ConcurrentBodyEdits_NonOverlapping(t *testing.T) {
	base := "## Deploy\nstep1\nstep2\nstep3\n"
	ours := "## Deploy\nstep1-NEW-FROM-PC1\nstep2\nstep3\n"
	theirs := "## Deploy\nstep1\nstep2\nstep3-NEW-FROM-PC2\n"

	got, conflict := Merge(base, ours, theirs)
	if conflict {
		t.Fatalf("non-overlapping edits should merge cleanly; got=%q", got)
	}
	if !strings.Contains(got, "step1-NEW-FROM-PC1") || !strings.Contains(got, "step3-NEW-FROM-PC2") {
		t.Errorf("expected both edits present: %q", got)
	}
}

// Overlapping edits to one line keep both versions (ours first) and never
// leave conflict markers in MEMORY.md.
func TestMerge_ConcurrentBodyEdits_Conflicting(t *testing.T) {
	base := "## Deploy\nstep1\n"
	ours := "## Deploy\nstep1-PC1\n"
	theirs := "## Deploy\nstep1-PC2\n"

	got, conflict := Merge(base, ours, theirs)
	if conflict {
		t.Errorf("overlapping line edits should keep both versions, not conflict; got=%q", got)
	}
	for _, marker := range []string{"<<<<<<<", "=======", ">>>>>>>"} {
		if strings.Contains(got, marker) {
			t.Errorf("unexpected conflict marker %q in output: %q", marker, got)
		}
	}
	if !strings.Contains(got, "step1-PC1") || !strings.Contains(got, "step1-PC2") {
		t.Errorf("expected both versions kept: %q", got)
	}
	if strings.Index(got, "step1-PC1") > strings.Index(got, "step1-PC2") {
		t.Errorf("expected ours before theirs: %q", got)
	}
}

func TestMerge_PreambleEdits(t *testing.T) {
	base := "# Project\nIntro v1.\n"
	ours := "# Project\nIntro v1 with extra ours.\n"
	theirs := "# Project\nIntro v1.\n\n## New\nadded by theirs.\n"

	got, conflict := Merge(base, ours, theirs)
	if conflict {
		t.Fatalf("non-overlapping preamble + new section should merge; got=%q", got)
	}
	if !strings.Contains(got, "extra ours") {
		t.Errorf("preamble edit lost: %q", got)
	}
	if !strings.Contains(got, "## New") {
		t.Errorf("new section lost: %q", got)
	}
}

// TestMerge_RealisticRockBotShape uses a sample shaped like the actual
// rockbot MEMORY.md from the user's machine — H1 title plus several H2
// sections containing rich markdown. Both sides add a new section and one
// side updates an existing section's body.
func TestMerge_RealisticRockBotShape(t *testing.T) {
	base := `# RockBot Project Memory

## Blazor Docker Build — Critical Rule
**Never use ` + "`--no-restore`" + ` on dotnet publish for Blazor projects.**

## Deploy Workflow
- Image: rockylhotka/rockbot-blazor:latest
- Helm: helm upgrade rockbot ...
`

	ours := `# RockBot Project Memory

## Blazor Docker Build — Critical Rule
**Never use ` + "`--no-restore`" + ` on dotnet publish for Blazor projects.**

## Deploy Workflow
- Image: rockylhotka/rockbot-blazor:latest
- Helm: helm upgrade rockbot ...
- Restart: kubectl rollout restart ...

## Agent Directives
PVC files survive image updates.
`

	theirs := `# RockBot Project Memory

## Blazor Docker Build — Critical Rule
**Never use ` + "`--no-restore`" + ` on dotnet publish for Blazor projects.**
The app builds but blazor.web.js 404s.

## Deploy Workflow
- Image: rockylhotka/rockbot-blazor:latest
- Helm: helm upgrade rockbot ...

## Cluster Auth
kubectl context: rockbot-prod.
`

	got, conflict := Merge(base, ours, theirs)
	if conflict {
		t.Fatalf("expected clean merge of disjoint changes, got=%q", got)
	}
	for _, want := range []string{
		"## Blazor Docker Build",
		"## Deploy Workflow",
		"## Agent Directives",     // ours-only
		"## Cluster Auth",         // theirs-only
		"blazor.web.js 404s",      // body edit from theirs
		"kubectl rollout restart", // list addition from ours
	} {
		if !strings.Contains(got, want) {
			t.Errorf("merged result missing %q\nGOT:\n%s", want, got)
		}
	}
}
