# claude-utils

[![CI](https://github.com/MarimerLLC/claude-utils/actions/workflows/ci.yml/badge.svg)](https://github.com/MarimerLLC/claude-utils/actions/workflows/ci.yml)

Cross-workstation tooling for Claude Code.

Two capabilities, both built from a single pair of Go binaries
(`claude-memsync` + `claude-memmerge`) that run on Windows, Linux, and macOS:

- **[claude-memsync](docs/claude-memsync.md)** — a background daemon that keeps
  your Claude Code memories (`~/.claude/projects/<hash>/memory/`) in sync across
  multiple workstations, using a private git repository as transport. A custom
  merge driver unions `MEMORY.md` section blocks instead of producing line-level
  conflicts.
- **[Distilling environment memories](docs/distilling-memories.md)** — lift the
  *transferable* lessons (shell/OS quirks, CLI gotchas, toolchain, your standing
  preferences) out of one project and reuse them in any other, existing or new,
  via a shared catalog and two Claude skills (`/distill`, `/distill-apply`).

## Quick start

```sh
# Build both binaries (they must end up in the same directory)
git clone https://github.com/MarimerLLC/claude-utils.git
cd claude-utils
go build -o bin/ ./cmd/...

# Bootstrap and run the sync daemon (per PC)
claude-memsync init --remote https://github.com/<you>/claude-sync.git
claude-memsync install
claude-memsync start
```

Full instructions:

- **Syncing memories across machines** → [docs/claude-memsync.md](docs/claude-memsync.md)
  (prerequisites, setup, lifecycle commands, how it works, deletes, auth,
  limitations).
- **Distilling lessons across projects** → [docs/distilling-memories.md](docs/distilling-memories.md)
  (installing the skills, the `/distill` and `/distill-apply` workflows,
  permissions, troubleshooting).

## Project layout

| Path | Purpose |
|---|---|
| `cmd/claude-memsync` | Daemon binary: lifecycle + watcher + sync loop |
| `cmd/claude-memmerge` | Git custom merge driver for `MEMORY.md` |
| `internal/sync` | Mirror, reconcile, watcher loop, manifest |
| `internal/merge` | Section-block parser + 3-way semantic merge |
| `internal/distill` | Distilled-memory catalog index + reconcile |
| `internal/gitwt` | Wrapper around the `git` CLI scoped to the sync work-tree |
| `internal/config` | Config load/save (`~/.claudesync/config.json`) |
| `internal/proc` | Cross-platform helper that hides child-process console windows on Windows |
| `internal/version` | Version resolution from `-ldflags` override or embedded VCS info |
| `skills/` | `/distill` and `/distill-apply` Claude Code skills |
| `.githooks/` | Shared git hooks (pre-commit gofmt check) |

## Development

The repo ships a pre-commit hook that runs the same `gofmt` check as CI, so
formatting problems surface locally instead of after a push. Enable it once per
clone:

```sh
git config core.hooksPath .githooks
```

It only inspects staged `.go` files and blocks the commit if any are not
gofmt-clean, printing the `gofmt -w …` command to fix them. (`go vet` and the
test suite stay in CI, which runs them across Linux/macOS/Windows.)

## Releasing

Versioning follows [SemVer](https://semver.org): `vMAJOR.MINOR.PATCH`.
We're pre-1.0, which means the daemon's behavior, config format, and
on-disk layout may still change between minor versions; bumping the
patch is reserved for fixes that don't change observable behavior.

Bump rules while on `0.x`:

| Change | Bump |
|---|---|
| Bug fix, internal refactor, doc update | patch (`v0.1.7` → `v0.1.8`) |
| Behavior change, new feature, default change, new config field | minor (`v0.1.x` → `v0.2.0`) |
| Breaking change to config format, on-disk layout, or CLI surface | minor while pre-1.0 — but call it out clearly in the tag message |

After 1.0 the standard SemVer rules kick in (breaking → major, additive → minor, fix → patch). At that point a `v2+` would also need a `/v2` suffix in the Go module path.

### Cutting a release

1. Land all changes on `main` and confirm CI is green.
2. Create a GitHub Release for the version. Either:

   ```sh
   gh release create v0.1.8 \
     --title "v0.1.8" \
     --notes "- <bullet summary of user-visible changes>" \
     --target main
   ```

   …or use the GitHub UI (Releases → Draft a new release → choose tag
   `v0.1.8`, target `main`, fill in title and notes, **Publish**).

3. Publishing the Release fires `.github/workflows/release.yml`, which
   cross-compiles both binaries for {linux, darwin, windows} ×
   {amd64, arm64}, stamps the version in via `-ldflags`, packages each
   target as `claude-utils-v0.1.8-<os>-<arch>.{tar.gz,zip}`, generates
   `SHA256SUMS`, and uploads everything to the Release. ~1 minute.

4. Watch the run finish:

   ```sh
   gh run watch
   gh release view v0.1.8     # confirms the assets are attached
   ```

5. To upgrade your own machine, download the appropriate archive from
   the Release page, extract, and swap the binaries:

   ```sh
   claude-memsync stop
   # extract archive, copy bin/* to your install location
   claude-memsync start
   claude-memsync version     # confirms the upgrade landed
   ```

### If the release workflow didn't fire

GitHub's `release: published` event is occasionally missed — usually
when the Release was created from a pre-existing tag via API. Two
recovery paths:

- **Manual dispatch** (preferred): trigger the workflow against the
  existing tag without touching the Release.

  ```sh
  gh workflow run release.yml --field tag=v0.1.8
  gh run watch
  ```

  This re-uses the same build/upload steps and the existing Release
  picks up the assets.

- **Recreate the Release**: `gh release delete v0.1.8 --cleanup-tag=false`
  then recreate it. Sometimes nudges GitHub into firing the event
  properly. Manual dispatch is simpler — prefer it unless you also
  need to fix release notes.

The same `workflow_run` invocation works for retroactively building
archives for any tag that doesn't yet have a Release, or for any
Release whose previous build run failed.

### Building locally without a release

For dev iteration, a plain `go build -o bin/ ./cmd/...` is enough — the
binaries will report a VCS-derived `dev+<sha>` version. To stamp a
specific version into a local build:

```sh
VERSION=$(git describe --tags --always --dirty)
go build -ldflags "-X github.com/MarimerLLC/claude-utils/internal/version.Override=$VERSION" -o bin/ ./cmd/...
```

PowerShell equivalent:

```powershell
$VERSION = git describe --tags --always --dirty
go build -ldflags "-X github.com/MarimerLLC/claude-utils/internal/version.Override=$VERSION" -o bin\ .\cmd\...
```

### How version resolution works

`internal/version.String()` returns the first of:

1. The build-time `Override` set via `-ldflags "-X .../internal/version.Override=<v>"` (preferred for releases).
2. `runtime/debug.BuildInfo.Main.Version` — populated automatically when installed via `go install <path>@<tag>`. Yields `v0.1.7` for tagged installs, or a pseudo-version like `v0.0.0-20260508213755-a52d68630b31` for untagged commits.
3. `vcs.revision` from the embedded build settings — yields `dev+<short-sha>` (or `dev+<short-sha>-dirty` if the working tree had uncommitted changes at build time).
4. The literal `dev` — only seen if Go embedded no VCS info (e.g. building from outside a git checkout).

This means a plain `go build` always produces a binary whose `version` subcommand says something useful — no Makefile or build script required for development.

## License

MIT — see [LICENSE](LICENSE).
