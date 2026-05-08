# claude-utils

Cross-workstation tooling for Claude Code.

## claude-memsync

Background daemon that keeps `~/.claude/projects/<project-hash>/memory/`
in sync across multiple workstations using a private git repository as
transport. A custom git merge driver (`claude-memmerge`) unions
`MEMORY.md` section blocks instead of producing line-level conflicts.
Single Go binary; runs on Windows, Linux, and macOS.

## Prerequisites

- Go 1.23+ to build
- `git` 2.x on PATH at runtime (the daemon shells out)
- A private git remote you can `git push` to from your terminal — the
  daemon inherits your normal git credentials (Git Credential Manager on
  Windows, SSH agent or `~/.gitconfig` credential helpers on Linux/macOS).
  Confirm `git push` works against the remote before installing.
- The same project paths on each PC (we use Claude's project-hash
  directory names, which are derived from absolute paths). If your repos
  live at the same drive letter / mount point on every PC, you're set.

## Setup

### 1. Create the private remote (once, from any PC)

Any empty private git repo works. With the GitHub CLI:

```sh
gh repo create <you>/claude-sync --private --description "Private Claude Code memory sync"
```

### 2. Build (each PC)

```sh
git clone https://github.com/MarimerLLC/claude-utils.git
cd claude-utils
go build -o bin/ ./cmd/...
```

Both binaries (`claude-memsync` and `claude-memmerge`) end up in `bin/`.
**They must live in the same directory** — `claude-memsync` finds the
merge driver as a sibling. Move both to a stable location if you don't
want them tied to the source checkout, e.g.:

- Windows: `C:\Program Files\claude-memsync\` (or anywhere on PATH)
- Linux/macOS: `~/.local/bin/`

### 3. Initialize the local sync repo (each PC)

```sh
claude-memsync init --remote https://github.com/<you>/claude-sync.git
```

This:

- Clones the remote into `~/.claudesync/`
- Configures the `MEMORY.md` merge driver in the local git config
- Mirrors any existing `~/.claude/projects/<hash>/memory/` content into
  the work-tree
- Writes `~/.claudesync/config.json` (per-PC; never synced)
- Writes `~/.claudesync/.state/manifest.json` (per-PC; never synced)
- Pushes the seed commit if this is the first PC

On a second or third PC where the remote already has content from
another workstation, init handles collisions:

- `MEMORY.md` present on both sides → semantic merge (sections from
  both PCs are unioned)
- Other memory files differing on both sides → mirror copy is preserved
  as `<name>.from-remote-<random>` for manual review; the local version
  is taken

### 4. Install and start the daemon (each PC)

```sh
claude-memsync install
claude-memsync start
claude-memsync status
```

`install` registers the daemon to auto-start when you log in:

- **Windows**: drops `claude-memsync.cmd` in
  `%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\`. No admin
  required. (Task Scheduler was investigated; it requires admin even for
  per-user logon tasks, so the Startup folder is the simpler path.)
- **Linux**: writes a systemd user unit at
  `~/.config/systemd/user/claude-memsync.service` and enables it.
- **macOS**: writes a launchd plist at
  `~/Library/LaunchAgents/claude-memsync.plist`.

All three run as your logged-in user, with full access to your
credential vault and SSH keys. None require root.

## Lifecycle commands

| Command | What it does |
|---|---|
| `claude-memsync init --remote <url>` | One-time bootstrap. |
| `claude-memsync install` | Register for auto-start at next logon. |
| `claude-memsync uninstall` | Remove the auto-start hook. |
| `claude-memsync start` | Start the daemon now. |
| `claude-memsync stop` | Stop the running daemon. |
| `claude-memsync status` | Print `running (pid N)` or `stopped`. |
| `claude-memsync run` | Run in the foreground (for debugging). |
| `claude-memsync version` | Print version. |

## How it works

```
~/.claude/projects/<hash>/memory/         (Claude reads + writes here — authoritative)
                  │
                  │  fsnotify watcher, 3s debounce
                  ▼
~/.claudesync/projects/<hash>/memory/     (mirror — git work-tree)
                  │
                  │  git add -A, commit, pull --rebase, push
                  ▼
                <remote>                  (private GitHub repo)
                  │
                  │  pull on 30s timer or after local push
                  ▼
   propagate pull-driven changes back to ~/.claude/projects/...
```

- The daemon never edits files in `~/.claude/projects/...` while a write
  is in progress — it copies into the mirror first, then any inbound
  changes from a `git pull` are written back atomically.
- The custom merge driver is registered in the local repo config and
  invoked by git via `.gitattributes` (`MEMORY.md merge=claude-memory-index`).
- Conflicts in non-`MEMORY.md` files surface as standard git conflict
  markers in the affected file. Rare in practice because each memory
  file has a unique filename per topic.

## What gets synced (and what doesn't)

**Synced:** every file directly under
`~/.claude/projects/<project-hash>/memory/` on any PC.

**Not synced:**

- `~/.claude/CLAUDE.md` (your global instructions)
- `~/.claude/agents/`, `~/.claude/commands/`, `~/.claude/skills/`
- `~/.claude/sessions/`, `~/.claude/todos/`, `~/.claude/cache/`,
  `~/.claude/history.jsonl`, `~/.claude/settings.json`, etc.
- `~/.claudesync/config.json` — per-PC (paths embed your machine layout)
- `~/.claudesync/.state/manifest.json` — per-PC (delete-detection state)
- `~/.claudesync/daemon.pid` — runtime state

If you want any of the additional `~/.claude/` content synced, that's a
future enhancement.

## Deletes

Deletes propagate across PCs. The daemon keeps a per-PC manifest
(`~/.claudesync/.state/manifest.json`) listing which Claude-side memory
files were present at the last successful sync. On each reconcile pass,
a file that's in the mirror, missing from Claude, **and** was in the
manifest is treated as a user delete and removed from the mirror; the
next push propagates it. A file that's in the mirror, missing from
Claude, but **not** in the manifest is treated as an inbound new file
from another PC and copied into Claude.

This means:

- Delete a memory while the daemon is running → propagates immediately
  (watcher catches it).
- Delete a memory while the daemon is stopped → propagates on next
  startup (manifest-driven reconcile).
- First-ever run on a PC has no manifest, so deletes can't be inferred
  from prior state. The daemon takes the safe path: never infer deletes,
  bring everything together. Subsequent runs work normally.

## On-disk layout

```
~/.claude/projects/<hash>/memory/         # what Claude reads + writes
    ├── MEMORY.md
    └── <topic>.md ...

~/.claudesync/                            # owned by the daemon
    ├── config.json                       # per-PC (gitignored)
    ├── daemon.pid                        # per-PC (gitignored)
    ├── .git/                             # the sync repo
    ├── .gitattributes                    # MEMORY.md merge=claude-memory-index
    ├── .gitignore                        # excludes config.json, .state/, etc.
    ├── .state/manifest.json              # per-PC delete-detection state
    └── projects/<hash>/memory/...        # git work-tree mirror
```

## Auth notes

The daemon shells out to the system `git` binary, so it uses whatever
auth is configured in your environment:

- **HTTPS with Git Credential Manager** (`gh auth login` on Windows
  populates this): zero extra setup.
- **SSH**: ensure `ssh-agent` is running for your user session and your
  key is loaded. On Linux, `systemctl --enable-linger <user>` keeps the
  user instance running across logoffs if you want sync activity while
  not logged in.
- **PAT in URL** (`https://<token>@github.com/...`): works but the token
  ends up in `~/.claudesync/.git/config`. Not recommended.

Test before installing the daemon:
```sh
git -C ~/.claudesync push
```
If that works without prompting, the daemon will too.

## Limitations / known issues

- **Path consistency required**: Claude derives the per-project memory
  directory name by escaping the project's absolute path. PCs that
  open the same repo at different paths (e.g. `C:\src\foo` vs.
  `D:\dev\foo`) will see them as different projects.
- **No live conflict UI**: when the merge driver emits actual conflict
  markers (rare; only on overlapping line edits within the same
  `MEMORY.md` section body), the file is committed and pushed with
  markers. The next time you edit on that PC you'll see them; resolve
  by hand and save.
- **Auto-start runs while logged in**: the daemon stops when the user
  logs off. Acceptable since memories don't change while you're away.
  For 24/7 sync, register a system-wide service manually or enable
  systemd lingering.
- **Stop is a hard kill**: by design — git operations are atomic per
  command, so we don't risk corruption. If a `.git/index.lock` is left
  behind by an unrelated git crash, remove it manually.

## Project layout

| Path | Purpose |
|---|---|
| `cmd/claude-memsync` | Daemon binary: lifecycle + watcher + sync loop |
| `cmd/claude-memmerge` | Git custom merge driver for `MEMORY.md` |
| `internal/sync` | Mirror, reconcile, watcher loop, manifest |
| `internal/merge` | Section-block parser + 3-way semantic merge |
| `internal/gitwt` | Wrapper around the `git` CLI scoped to the sync work-tree |
| `internal/config` | Config load/save (`~/.claudesync/config.json`) |

## License

MIT — see [LICENSE](LICENSE).
