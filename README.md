# claude-utils

Cross-workstation tooling for Claude Code.

## claude-memsync

Background daemon that keeps `~/.claude/projects/*/memory/` in sync across
multiple workstations using a private git repository as transport. Supports
Windows (Service Control Manager), Linux (systemd), and macOS (launchd) via a
single Go binary.

Conflicts in `MEMORY.md` are resolved by a semantic merge driver
(`claude-memmerge`) that unions section blocks rather than relying on git's
line-based 3-way merge. Individual memory files use git's default merge.

### Layout

- `cmd/claude-memsync` — the daemon binary (file watcher + git sync loop + service lifecycle)
- `cmd/claude-memmerge` — the git custom merge driver for `MEMORY.md`
- `internal/sync` — debounced watch/commit/pull/push loop
- `internal/merge` — section-block parser + union logic for MEMORY.md
- `internal/gitwt` — wrapper around `git` against the sync work-tree
- `internal/config` — TOML config at `~/.claudesync/config.toml`

### On-disk layout

We keep all sync state under `~/.claudesync/`, separate from `~/.claude/` so
Claude Code can evolve its own directory structure without colliding with us.
The daemon mirrors `~/.claude/projects/<hash>/memory/` files into
`~/.claudesync/projects/<hash>/memory/`, which is the git work-tree:

```
~/.claudesync/
├── config.toml             daemon config (remote, debounce, paths)
├── .git/                   sync repo's git directory
├── .gitattributes          registers MEMORY.md merge driver
└── projects/               git work-tree mirror
    └── <hash>/memory/MEMORY.md
```

The authoritative copy of memories is always `~/.claude/projects/<hash>/memory/`;
the daemon never commits while a Claude write is in progress because it copies
into the mirror first.

### Status

Under construction.
