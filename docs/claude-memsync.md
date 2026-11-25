# claude-memsync — syncing Claude Code memories across workstations

A guide to setting up and running the memory-sync daemon.

## What this is for

Claude Code stores per-project memories under
`~/.claude/projects/<project-hash>/memory/`. If you work across more than one
machine, those memories live only on whichever PC learned them. `claude-memsync`
is a small background daemon that keeps that memory tree in sync across all your
workstations, using a **private git repository as transport**. A custom git
merge driver (`claude-memmerge`) unions `MEMORY.md` section blocks instead of
producing line-level conflicts.

It's a single Go binary, runs on Windows, Linux, and macOS, and needs no server
of its own — just a git remote you can push to.

> Want to reuse environment-level lessons across *different* projects (not just
> sync the same project across machines)? That's a separate capability — see
> [distilling-memories.md](distilling-memories.md).

## Prerequisites

- Go 1.23+ to build.
- `git` 2.x on PATH at runtime (the daemon shells out to it).
- A private git remote you can `git push` to from your terminal. The daemon
  inherits your normal git credentials (Git Credential Manager on Windows, SSH
  agent or `~/.gitconfig` credential helpers on Linux/macOS). Confirm `git push`
  works against the remote before installing.
- Repos checked out at **different paths on different PCs still merge**: the
  daemon fingerprints each project (by its git remote URL, falling back to the
  root-commit SHA) and keys the sync repo by that fingerprint rather than the
  local path. Non-git directories keep the old path-based key and only merge
  across PCs that use identical paths. (See [How it works](#how-it-works) and
  [Limitations](#limitations--known-issues).)

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

For a release build that stamps the version into the binary:

```sh
VERSION=$(git describe --tags --always --dirty)
go build -ldflags "-X github.com/MarimerLLC/claude-utils/internal/version.Override=$VERSION" -o bin/ ./cmd/...
```

A plain `go build` (no ldflags) still produces a working binary —
`claude-memsync version` falls back to the VCS revision Go embeds automatically
(e.g. `dev+a52d68609b6e`). See the README's *Releasing* section for details.

Both binaries (`claude-memsync` and `claude-memmerge`) end up in `bin/`.
**They must live in the same directory** — `claude-memsync` finds the merge
driver as a sibling. Move both to a stable location if you don't want them tied
to the source checkout, e.g.:

- Windows: `C:\Program Files\claude-memsync\` (or anywhere on PATH)
- Linux/macOS: `~/.local/bin/`

### 3. Initialize the local sync repo (each PC)

```sh
claude-memsync init --remote https://github.com/<you>/claude-sync.git
```

This:

- Clones the remote into `~/.claudesync/`
- Configures the `MEMORY.md` merge driver in the local git config
- Mirrors any existing `~/.claude/projects/<hash>/memory/` content into the
  work-tree
- Writes `~/.claudesync/config.json` (per-PC; never synced)
- Writes `~/.claudesync/.state/manifest.json` (per-PC; never synced)
- Pushes the seed commit if this is the first PC

On a second or third PC where the remote already has content from another
workstation, init handles collisions:

- `MEMORY.md` present on both sides → semantic merge (sections from both PCs are
  unioned)
- Other memory files differing on both sides → mirror copy is preserved as
  `<name>.from-remote-<random>` for manual review; the local version is taken

### 4. Install and start the daemon (each PC)

```sh
claude-memsync install
claude-memsync start
claude-memsync status
```

`install` registers the daemon to auto-start when you log in:

- **Windows**: drops `claude-memsync.vbs` in
  `%APPDATA%\Microsoft\Windows\Start Menu\Programs\Startup\`. The script launches
  the daemon hidden and detached via `WScript.Shell.Run`, so no console window is
  left behind at logon. No admin required. (Task Scheduler was investigated; it
  requires admin even for per-user logon tasks, so the Startup folder is the
  simpler path.)
- **Linux**: writes a systemd user unit at
  `~/.config/systemd/user/claude-memsync.service` and enables it.
- **macOS**: writes a launchd plist at
  `~/Library/LaunchAgents/claude-memsync.plist`.

All three run as your logged-in user, with full access to your credential vault
and SSH keys. None require root.

## Lifecycle commands

| Command | What it does |
|---|---|
| `claude-memsync init --remote <url>` | One-time bootstrap. |
| `claude-memsync install` | Register for auto-start at next logon. |
| `claude-memsync uninstall` | Remove the auto-start hook. |
| `claude-memsync start` | Start the daemon now. |
| `claude-memsync stop` | Stop the running daemon. |
| `claude-memsync status [--config <path>]` | Print `running (pid N)` or `stopped`, then a sync-health line (see [Checking sync health](#checking-sync-health)). |
| `claude-memsync run` | Run in the foreground (for debugging). |
| `claude-memsync distill` | Rebuild the distilled-memory catalog index (`--prune`, `--dry-run`). See [distilling-memories.md](distilling-memories.md). |
| `claude-memsync version` | Print version. |

## How it works

```
~/.claude/projects/<localHash>/memory/    (Claude reads + writes here — authoritative)
                  │                        localHash = path with / → -, per machine
                  │  fsnotify watcher, 3s debounce
                  │  fingerprint the project → stable key
                  ▼
~/.claudesync/projects/<key>/memory/       (mirror — git work-tree, keyed by fingerprint)
                  │
                  │  git add -A, commit, pull (merge), resolve conflicts, push
                  ▼
                <remote>                   (private GitHub repo)
                  │
                  │  ls-remote check on 1h timer (or after local push);
                  │  full pull only when remote SHA actually moved
                  ▼
   propagate pull-driven changes back to ~/.claude/projects/<localHash>/...
```

**Project fingerprinting.** Claude names each project directory by dash-encoding
its absolute path (`/Users/me/code/foo` → `-Users-me-code-foo`), which differs
per machine. Keying the sync repo by that name would split one repo into several.
Instead the daemon resolves each project's real path (from the `cwd` Claude
records in its session transcripts) and derives a machine-independent **key**:

- `g-<hash>` — from the normalized `origin` remote URL (all of
  `git@github.com:Acme/app.git`, `https://github.com/acme/app`, and
  `…/app.git` collapse to the same key).
- `r-<hash>` — from the git root-commit SHA, when the repo has no remote.
- the dash-encoded path itself — fallback for non-git directories (unchanged
  behavior; only merges across PCs with identical paths).

A project opened in a **subdirectory** of a repo (Claude keeps a separate
project per directory) gets its own key: the path relative to the repo root is
folded into the hash, so `repo/docs` doesn't share memories with `repo`, and a
plain folder that only sits inside some enclosing repo (a dotfiles-managed
home, say) doesn't take on that repo's identity.

Several local projects can map to one key — the same repo checked out twice on
one machine, e.g. as **git worktrees**. They share one memory set: an edit in
one checkout is copied to the others straight away, and reconcile keeps them
identical. If two checkouts change the same file before the daemon catches up,
nothing is overwritten: the next flush reconciles them (`MEMORY.md` merged,
other files kept with a `.from-remote-*` backup), and an edit outranks a
delete of the same file.

A project opened here for the first time is reconciled before its first local
write goes to the mirror, so memories already synced from another PC are
merged in rather than overwritten.

Remote URLs keep a non-default port (`git.example:2222/…` is not
`git.example/…`), and the subdirectory part of a key follows each directory's
name as stored on disk — so `docs` and `Docs` stay separate on a case-sensitive
filesystem, while a cwd typed with different casing on Windows or macOS still
matches.

A per-PC index at `~/.claudesync/.state/index.json` (gitignored) records the
`localHash → key` mapping. It routes inbound changes back to the right local
directories and keeps keys stable: a project whose checkout has since gone away
(removed worktree, unmounted drive, transcripts cleaned up by Claude) keeps its
last key rather than falling back to the path key. Projects whose transcripts
haven't changed since the last pass are not re-fingerprinted, so the periodic
rebuild stays cheap.

**Lazy materialization.** A project synced from another PC but never opened on
this one stays pending in the mirror — there's no local path to write it to yet.
The first time you open that project here, its directory appears, fingerprints to
the pending key, and the next reconcile copies its memories in.

Idle ticks cost a single `git ls-remote` round-trip — no pull, no push, no merge
driver invocation. The full pull/push cycle runs only when origin's branch SHA
differs from the local `refs/remotes/origin/<branch>` or when there are unpushed
commits.

- The daemon never edits files in `~/.claude/projects/...` while a write is in
  progress — it copies into the mirror first, then any inbound changes from a
  `git pull` are written back atomically.
- The custom merge driver is registered in the local repo config and invoked by
  git via `.gitattributes` (`MEMORY.md merge=claude-memory-index`).
- The pull always **merges** (`git pull --no-rebase`), so a conflict is met
  once, however many local commits are waiting. See
  [Conflicts](#conflicts) for how each kind is settled.

## Conflicts

When a pull hits a conflict, the daemon settles every conflicted file itself,
commits the merge and pushes. A conflict never leaves the sync repo stuck, and
never leaves conflict markers in a file.

| Conflict | Result |
|---|---|
| `MEMORY.md`, both PCs edited it | Merged section by section. Where both edited the same lines, **both versions are kept** (this PC's first). Done in-process, so it works even if the merge driver isn't configured. |
| Other memory file, both PCs edited it | The frontmatter `modified:` timestamp (top-level or under `metadata:`) is set to the newer of the two on every version first, so the timestamp alone never conflicts. If the edits don't overlap, they merge cleanly with **no backup**. Otherwise the side with the **newer `modified:` wins** each overlapping section (the other side's non-overlapping edits are kept); if either side has no timestamp, or they're equal, **this PC wins**. |
| Both PCs created a file with the same name | Same as above, with an empty common ancestor. |
| Binary file (contains a NUL byte), both PCs edited it | Can't be merged by line: the winner, chosen by the same timestamp rule, is kept as a whole file. |
| Edited on one PC, deleted on the other | The edited copy is kept (an edit outranks a delete). |
| Renamed differently on each PC | Both new names are kept; the old name is removed. |

Whenever one side's version loses, its original is saved next to the file in
the mirror as `<name>.from-remote-<random>` (in
`~/.claudesync/projects/<key>/memory/`), once the merge is committed.
Backups are git-ignored: they stay on the PC that did the merge for you to
review, and are never synced or copied into `~/.claude/projects`. The daemon
log names each resolved file and backup.

If resolving fails for any reason, the merge is aborted (the repo goes back to
its pre-pull state, local commits intact), the failure is recorded, and the
next sync tries again. A merge or rebase left half-done (by an older daemon
that stopped mid-sync, or by hand) is aborted the same way at startup and
before every sync. If it can't be aborted (a stale `.git/index.lock`, say),
the daemon stages and commits nothing until it can, and `status` shows the
failure.

## Checking sync health

`claude-memsync status` prints a second line about how syncing is going,
from `~/.claudesync/.state/health.json` and the local copy of origin's branch
(it makes no network call):

```
running (pid 12345)
sync: ok — last synced 2026-11-24 17:50 (up to date with origin)
```

```
running (pid 12345)
sync: FAILING — 412 consecutive failures since 2026-08-19 17:01 (ahead 231, behind 360)
  last success: 2026-08-19 16:00
  last error: pull: git pull …: …
```

If the daemon was installed with `install --config <path>`, pass the same
`--config <path>` to `status`; otherwise it reads `~/.claudesync/config.json`.

"Last synced" is the last time the daemon confirmed this PC matches origin
(an idle check counts). A failure is any sync that couldn't finish: origin
unreachable, push rejected, a conflict that couldn't be resolved, or local
memories that couldn't be copied into the mirror. The exit code
of `status` stays 0 either way, so existing scripts keep working.

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
- `~/.claudesync/.state/index.json` — per-PC (localHash → fingerprint key map)
- `~/.claudesync/.state/migrated.json` — per-PC (legacy files already imported)
- `~/.claudesync/.state/health.json` — per-PC (sync health for `status`)
- `.from-remote-*` conflict backups — per-PC, for review
- `~/.claudesync/daemon.pid` — runtime state
- `~/.claudesync/distilled/DISTILLED.md` — derived index, regenerated per-PC
  (the distilled `<slug>.md` entry files themselves **are** synced)

If you want any of the additional `~/.claude/` content synced, that's a future
enhancement.

## Deletes

Deletes propagate across PCs. The daemon keeps a per-PC manifest
(`~/.claudesync/.state/manifest.json`) listing which Claude-side memory files
were present at the last successful sync. On each reconcile pass, a file that's
in the mirror, missing from Claude, **and** was in the manifest is treated as a
user delete and removed from the mirror; the next push propagates it. A file
that's in the mirror, missing from Claude, but **not** in the manifest is treated
as an inbound new file from another PC and copied into Claude.

This means:

- Delete a memory while the daemon is running → propagates immediately (watcher
  catches it).
- Delete a memory while the daemon is stopped → propagates on next startup
  (manifest-driven reconcile).
- First-ever run on a PC has no manifest, so deletes can't be inferred from prior
  state. The daemon takes the safe path: never infer deletes, bring everything
  together. Subsequent runs work normally.

## On-disk layout

```
~/.claude/projects/<localHash>/memory/    # what Claude reads + writes
    ├── MEMORY.md
    └── <topic>.md ...

~/.claudesync/                            # owned by the daemon
    ├── config.json                       # per-PC (gitignored)
    ├── daemon.pid                        # per-PC (gitignored)
    ├── .git/                             # the sync repo
    ├── .gitattributes                    # MEMORY.md merge=claude-memory-index
    ├── .gitignore                        # excludes config.json, .state/, etc.
    ├── .state/manifest.json              # per-PC delete-detection state
    ├── .state/index.json                 # per-PC localHash → fingerprint key map
    ├── .state/migrated.json              # per-PC record of imported legacy files
    ├── .state/health.json                # per-PC sync health, read by `status`
    ├── distilled/                        # shared distilled-memory catalog
    │   ├── DISTILLED.md                  # derived index (gitignored)
    │   └── <slug>.md ...                 # distilled entries (synced)
    └── projects/<key>/memory/...         # git work-tree mirror (keyed by fingerprint)
```

## Auth notes

The daemon shells out to the system `git` binary, so it uses whatever auth is
configured in your environment:

- **HTTPS with Git Credential Manager** (`gh auth login` on Windows populates
  this): zero extra setup.
- **SSH**: ensure `ssh-agent` is running for your user session and your key is
  loaded. On Linux, `systemctl --enable-linger <user>` keeps the user instance
  running across logoffs if you want sync activity while not logged in.
- **PAT in URL** (`https://<token>@github.com/...`): works but the token ends up
  in `~/.claudesync/.git/config`. Not recommended.

Test before installing the daemon:

```sh
git -C ~/.claudesync push
```

If that works without prompting, the daemon will too.

## Limitations / known issues

- **Git projects merge across paths; non-git ones don't**: git repos are keyed
  by their remote URL (or root-commit SHA), so opening the same repo at different
  paths on different PCs still merges. A non-git directory falls back to a
  path-based key, so it only merges across PCs that use the identical absolute
  path.
- **Upgrading from a path-keyed layout**: an upgraded daemon imports the existing
  path-keyed mirror directories into their fingerprint keys, merging any that
  collapse to the same key, and keeps importing edits that not-yet-upgraded PCs
  make to them (on every pull tick). It **leaves the legacy directories in
  place**: a PC still running the old version would read their deletion as you
  deleting those memories and remove its local copies. Upgrade PCs in any order;
  until a PC is upgraded it keeps syncing through the legacy directories and
  doesn't see changes other PCs make under the new keys. An old-version PC also
  copies the new `g-…`/`r-…` mirror dirs into its `~/.claude/projects/` as
  stray folders; they do no harm, are ignored after upgrading, and can be
  deleted.
- **Pruning legacy directories**: once **every** PC syncing the repo runs a
  fingerprint-aware version, set `"pruneLegacyDirs": true` in
  `~/.claudesync/config.json` and restart the daemon. It deletes each legacy
  file after importing it (and the directory once empty) and pushes the
  deletions; other upgraded PCs ignore them. A PC only prunes the legacy
  directories of its own project paths, so if your PCs use different paths, set
  it on each of them.
- **Fingerprint edge cases**: a shallow clone (`git clone --depth 1`) has no true
  root commit, so a remote-less shallow repo falls back to the path key. A repo
  whose `origin` URL is later changed (renamed org, new host) re-keys — the next
  time that project is used — and re-merges under the new key.
- **Conflicts are settled automatically, not interactively**: there's no prompt
  when two PCs edit the same lines. `MEMORY.md` keeps both versions (you may
  want to tidy duplicated lines); other files keep the newer version and save
  the other as a `.from-remote-*` backup on the PC that merged. Review backups
  now and then and delete them when done.
- **Auto-start runs while logged in**: the daemon stops when the user logs off.
  Acceptable since memories don't change while you're away. For 24/7 sync,
  register a system-wide service manually or enable systemd lingering.
- **Stop is a hard kill**: by design — git operations are atomic per command, so
  we don't risk corruption. If a `.git/index.lock` is left behind by an unrelated
  git crash, remove it manually.
