---
name: distill-apply
description: Seed environment-level lessons from the shared distilled catalog
  (~/.claudesync/distilled/) into the CURRENT project's Claude Code memory. Run
  from within whatever project — new or existing — you want to bring up to speed.
  Use when the user wants another project to benefit from lessons already learned
  elsewhere.
---

# distill-apply — seed distilled lessons into this project

The companion to `/distill`. Where `/distill` *promotes* transferable lessons
into the shared catalog, this *applies* chosen catalog entries into the current
project's memory so Claude here starts out knowing them.

Run this **from within the project you want to enrich** — its memory directory is
the one the harness lets you write to without prompting.

## Steps

1. **Refresh and read the catalog.** Run `claude-memsync distill` to regenerate
   the index, then read `~/.claudesync/distilled/DISTILLED.md` (or list the
   `*.md` entry files in `~/.claudesync/distilled/`, ignoring `DISTILLED.md`
   itself). Reading there should not prompt if `claude-memsync init` installed
   the allow-rule.

2. **Diff against this project.** Read the current project's memory directory
   (`~/.claude/projects/<hash>/memory/`, the path in the memory system-reminder).
   Match by `name`: skip catalog entries already present here. Present the
   remaining entries to the user — name + description — and let them pick, or
   apply all not-yet-present entries if they ask for everything.

3. **Seed each chosen entry.** Copy its body into a new file in *this* project's
   memory directory, named `<slug>.md`. Keep the frontmatter, but you may drop
   `originProject` / `originFile` (provenance is optional once seeded) and keep
   `scope: environment` so this project's `/distill` won't try to re-promote it.

4. **Update the index.** Add a one-line pointer to this project's `MEMORY.md`
   under an appropriate heading (e.g. a "Shared environment" section):
   `- [<Title>](<slug>.md) — <hook>`.

5. **Report** what you seeded and what you skipped as already-present.

## Notes

- This only seeds into the **current** project. To enrich a different project,
  run the skill from inside that project.
- Entries are environment-level by construction, so they apply broadly — but if
  one clearly doesn't fit this project's stack, say so and skip it rather than
  seeding noise.
