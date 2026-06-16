---
name: distill
description: Review this project's Claude Code memories, promote environment-level
  lessons (shell/OS quirks, CLI gotchas, toolchain, user identity) into the shared
  distilled catalog at ~/.claudesync/distilled/, and tag the originals. Run on
  demand. Use when the user wants to extract transferable lessons from a project
  so other projects can reuse them.
---

# distill — promote environment-level memories into the shared catalog

You are the *classifier of record* for the distilled-memory system. The
`claude-memsync` daemon can only mechanically aggregate and index what you
produce; the judgment lives here.

## Concept

Per-project memories live at `~/.claude/projects/<hash>/memory/*.md`. Some
lessons are **transferable** across every project (how the shell behaves, CLI
gotchas, the user's identity and standing preferences); most are **bound to one
repo** (its code, services, deploy, branch rules). This skill finds the
transferable ones, rewrites them to be project-neutral, and writes them into the
shared catalog so `/distill-apply` can seed them into any other project.

The catalog lives at **`~/.claudesync/distilled/`** (one `<slug>.md` file per
lesson). It is inside the `claude-memsync` work-tree, so entries sync across the
user's workstations automatically.

## Paths & environment — go straight there, don't go exploring

Work only with two known locations: the current project's memory directory and
`~/.claudesync/distilled/`. **Do not probe `$HOME`, `~/.claudesync/` (the
parent), or other directories to "orient"** — that triggers needless permission
prompts. In particular:

- The catalog directory may not exist yet. Don't test-and-search for it —
  create it directly with `mkdir -p ~/.claudesync/distilled` before writing, or
  just write the file (the write also creates the dir).
- `claude-memsync` may not be on PATH. Don't hunt for the binary. The index
  rebuild (step 8) is **best-effort**: if `claude-memsync` isn't found, skip it
  with a one-line note — the catalog entries you wrote are what matter, and the
  daemon (or a later manual run) regenerates the index.
- Writing under `~/.claudesync/distilled/` should not prompt if `claude-memsync
  init` installed the allow-rule. A prompt there just means the rule isn't in
  settings yet — proceed if the user approves; don't go looking elsewhere.

## Steps

1. **Scope.** Default to the *current* project's memory directory (the path
   shown in the memory system-reminder, e.g.
   `~/.claude/projects/<hash>/memory/`). Only widen to other projects if the
   user explicitly asks — cross-project reads may require permission.

2. **List candidates.** Read every `*.md` in that directory. Skip `MEMORY.md`,
   any `*.tmp.*` files, and any file that already has `scope: environment` in
   its frontmatter (already classified — leave it).

3. **Classify** each remaining memory against this rubric:

   | Verdict | Means | Examples |
   |---------|-------|----------|
   | **environment** | true in any repo on this machine/account | MINGW64 `kubectl cp` / `< redirect` are broken → use `cat \| kubectl exec -i`; `gh issue assign --self` doesn't exist → use `gh issue edit --add-assignee`; the user's mission/standing preferences |
   | **project** | tied to *this* repo | "this repo requires squash merges"; a service's deploy steps; routing/business logic; anything naming this project's components |

   When unsure, choose **project**. A false promotion pollutes every other
   project's context; a missed one just stays put until next time.

4. **Generalize** each environment memory before promoting it. Strip residual
   project specifics — replace a `rockbot`-specific pod selector or path with a
   generic placeholder or example, drop framing like "in this repo" — while
   preserving the transferable rule and a usable example. The catalog entry must
   read as advice for *any* project.

5. **Write the catalog entry** to `~/.claudesync/distilled/<slug>.md` where
   `<slug>` is the memory's `name` (run `mkdir -p ~/.claudesync/distilled` first
   if you're shelling out, or just write the file). Use exactly this frontmatter
   shape (the daemon's parser reads `metadata.*`):

   ```markdown
   ---
   name: <slug>
   description: <one-line summary>
   metadata:
     type: feedback        # or user / reference — carry over from the original
     scope: environment
     originProject: <hash dir name of the source project>
     originFile: <source file name>
   ---

   <generalized body>
   ```

6. **Tag the original** in place: add `scope: environment` under its `metadata:`
   block (or as a top-level key if it uses the older flat frontmatter). Leave the
   original body unchanged — the project keeps its richer, specific version. This
   tag is a breadcrumb: it marks the memory as promoted and lets future `/distill`
   runs skip it, and it lets `claude-memsync` prune the catalog entry if the
   original is later deleted.

7. **Resolve conflicts.** If `claude-memsync distill` (next step) reports the
   same `name` carried by multiple sources with divergent content, read both and
   write one merged, generalized entry.

8. **Rebuild the index and report (best-effort).** Try:

   ```sh
   claude-memsync distill
   ```

   If it runs, it regenerates `~/.claudesync/distilled/DISTILLED.md` and prints
   the entry count plus any pending (tagged-but-not-yet-distilled) memories and
   conflicts. **If `claude-memsync` isn't on PATH, don't search for it** — note
   that the index will be regenerated by the daemon (or a later manual
   `claude-memsync distill`) and move on. Either way, summarize for the user what
   you promoted, what you left as project-specific and why, and anything pending.

## Notes

- Do **not** invent a `scope` marker on the everyday memory-writer's behalf
  elsewhere; this skill is where that decision is made and recorded.
- Never merge prose mechanically — that's why this is a skill and not part of the
  Go daemon.
