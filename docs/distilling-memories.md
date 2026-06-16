# Distilling environment memories across projects

A practical guide to extracting the lessons Claude has learned in one project
and reusing them everywhere.

## What this is for

As you work with Claude Code in a project, it accumulates memories — small notes
about how to do things in your environment. Over months, some of those become
genuinely valuable and **transferable**: how your shell behaves, CLI flags that
don't exist, toolchain quirks, your standing preferences. But they're trapped in
one project's memory. Start a new repo and Claude re-learns them from scratch.

Distilling fixes that. You **distill** the transferable lessons out of a project
into a shared catalog, then **apply** them into any other project — existing or
brand new — so Claude starts out already knowing them.

What belongs in the catalog (environment-level):

- "On MINGW64, `kubectl cp` is broken; pipe with `cat … | kubectl exec -i`."
- "`gh issue assign --self` doesn't exist; use `gh issue edit --add-assignee`."
- Your mission, role, or standing preferences.

What does **not** (project-specific — stays where it is):

- "This repo requires squash merges."
- A service's deploy steps, routing logic, or anything naming this repo's
  components.

## How it works (the short version)

Three pieces, splitting judgment from mechanics:

| Piece | Kind | Job |
|-------|------|-----|
| `/distill` | Claude skill | Decide which memories are environment-level, rewrite them to be project-neutral, write catalog entries, tag the originals. |
| `claude-memsync distill` | Go CLI / daemon | Rebuild the catalog index, prune stale entries, report the worklist. No judgment — pure mechanics. |
| `/distill-apply` | Claude skill | Copy chosen catalog entries into the current project's memory. |

The catalog lives at `~/.claudesync/distilled/` — one `<slug>.md` file per
lesson, plus a generated `DISTILLED.md` index. Because it sits inside the
`claude-memsync` work-tree, entries sync across all your workstations
automatically.

## One-time setup

You only do this once per machine.

1. **Build and install the binaries** (see the main README for the full sync
   setup). You need `claude-memsync` on your PATH and a `claude-memsync init`
   already run.

2. **Install the skills** to your user scope so they work in every project:

   ```sh
   cp -r skills/distill skills/distill-apply ~/.claude/skills/
   ```

3. **Allow the skills to use the catalog without permission prompts.**
   `claude-memsync init` prints the rule; add it to `~/.claude/settings.json`:

   ```jsonc
   { "permissions": { "allow": [
     "Read(~/.claudesync/distilled/**)",
     "Write(~/.claudesync/distilled/**)"
   ] } }
   ```

   Without this, the skills still work but Claude will ask permission each time
   it reads or writes the catalog (expected in default, non-bypass mode).

## Workflow 1 — distill lessons out of a project

Do this in a project where Claude has learned things worth keeping (e.g. the one
you've worked in for months).

1. Open Claude Code in that project.
2. Run the skill:

   ```
   /distill
   ```

3. Claude reviews the project's memories, decides which are environment-level,
   rewrites them to drop project specifics, writes them into the catalog, and
   tags the originals `scope: environment` (so it won't re-process them next
   time). It then runs `claude-memsync distill` to refresh the index.
4. Claude reports what it promoted, what it left as project-specific and why,
   and anything still pending. Review its choices — if it promoted something too
   project-specific, tell it to revert that one.

You can re-run `/distill` any time; it skips already-classified memories, so it
only looks at what's new.

## Workflow 2 — apply lessons to another project

Do this **from inside the project you want to bring up to speed** — including a
brand-new one.

1. Open Claude Code in the target project.
2. Run:

   ```
   /distill-apply
   ```

3. Claude refreshes the catalog index, lists the entries not already present in
   this project, and lets you pick (or apply all). It copies the chosen entries
   into this project's memory and adds pointers to its `MEMORY.md`.
4. From now on, Claude in this project starts out knowing those lessons.

To enrich several projects, run `/distill-apply` in each.

## Keeping the catalog fresh

If the `claude-memsync` daemon is running, it rebuilds the catalog index
automatically after every sync — including entries that arrive from your other
workstations. You rarely need to think about it.

If you're not running the daemon, or want to refresh by hand:

```sh
claude-memsync distill            # rebuild DISTILLED.md, report worklist
claude-memsync distill --dry-run  # show what would change, write nothing
claude-memsync distill --prune    # also remove entries whose source is gone
```

The CLI prints:

- how many entries are in the catalog,
- **pending** memories — tagged `scope: environment` but not yet written to the
  catalog (run `/distill` to generalize them),
- **conflicts** — the same lesson distilled differently in two places (resolve
  in `/distill`).

## What a catalog entry looks like

```markdown
---
name: mingw-kubectl-file-transfer
description: On MINGW64 pipe files into kubectl exec with cat and -i
metadata:
  type: feedback
  scope: environment
  originProject: S--src-rdl-rockbot
  originFile: feedback_mingw_kubectl.md
---

On MINGW64 (Git Bash on Windows), `kubectl cp` and `< file` stdin redirects are
both broken. Pipe instead: `cat file | kubectl exec -i <pod> -- sh -c 'cat > /path'`.
```

`scope: environment` is what marks a memory for the catalog. You normally never
write it by hand — `/distill` decides and writes it. The daemon and CLI use it
to know what belongs.

## Troubleshooting

- **Claude keeps asking permission to read/write the catalog.** The allow-rule
  in setup step 3 isn't in place. Add it to `~/.claude/settings.json`.
- **`/distill` promoted something too project-specific.** Tell Claude to remove
  that entry from `~/.claudesync/distilled/` and un-tag the original; or delete
  the `<slug>.md` and run `claude-memsync distill --prune`.
- **An entry shows up as a conflict.** The same lesson was distilled differently
  on two machines or from two projects. Run `/distill` and ask Claude to merge
  them into one generalized entry.
- **A distilled lesson no longer applies.** Delete its `<slug>.md` from the
  catalog (or remove the `scope: environment` tag from the source and run
  `claude-memsync distill --prune`).
- **`DISTILLED.md` looks stale.** It's a derived file, regenerated locally; run
  `claude-memsync distill` to rebuild it. It is intentionally not synced (each PC
  regenerates its own to avoid merge conflicts on the generated table).

## Mental model

- **Entry files are the source of truth** and sync across your machines.
- **`DISTILLED.md` is derived** — regenerated locally, never synced.
- **`/distill` is the brain** — the only place classification and rewriting
  happen.
- **`claude-memsync distill` is the muscle** — it indexes and prunes, never
  judges.
- **`/distill-apply` is the courier** — it moves lessons into a project, one
  project at a time.
