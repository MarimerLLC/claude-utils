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

func TestMerge_ConcurrentBodyEdits_Conflicting(t *testing.T) {
	base := "## Deploy\nstep1\n"
	ours := "## Deploy\nstep1-PC1\n"
	theirs := "## Deploy\nstep1-PC2\n"

	got, conflict := Merge(base, ours, theirs)
	if !conflict {
		t.Errorf("expected conflict for overlapping line edits; got=%q", got)
	}
	if !strings.Contains(got, "<<<<<<<") {
		t.Errorf("expected conflict markers in output: %q", got)
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
