# Workspace home setup guide

How to set up a workspace's persistent `$HOME` (workspace home) and do its first login.
See [`docs/plans/home-workspace-volume.md`](../../plans/home-workspace-volume.md) for the
full design background (Japanese only for now).

## What is a workspace home

Every workspace has a dedicated, persistent directory at
`~/.local/share/boid/homes/<slug>/`. Every job (hook, exec, or session) that belongs to a
project assigned to that workspace mounts this directory as its sandbox `$HOME`,
read-write.

- **Persists across jobs**: files written to `$HOME` by one job (credentials, package
  caches, installed tools, ...) are still there for the next job in the same workspace
- **`$HOME/.boid` is the one exception**: it is used for the context/output file protocol
  and gets a fresh tmpfs layered on top for every single job. Nothing written there
  survives to the next job — a separate lifecycle from the rest of the workspace home
- **Not shared across workspaces**: workspace A's `$HOME` and workspace B's `$HOME` are
  different directories, and neither is shared with the real host `$HOME` (nor with the
  `boid` daemon process's own `$HOME`)

A project with no explicit workspace assignment uses the `default` workspace's home.

## Writing an init.sh

Placing an `init.sh` under a workspace's config makes it run automatically **on the first
dispatch into that workspace** — useful for one-time setup work in the workspace home,
such as installing the claude CLI.

### Location

```
~/.config/boid/workspaces/<slug>/init.sh
```

(`$XDG_CONFIG_HOME` takes precedence when set.) This lives alongside `workspace.yaml` and
`host_commands.yaml` in the host-side config directory — the sandbox can neither see nor
write to it.

A workspace with no `init.sh` is treated as "nothing to initialize" and dispatch proceeds
unchanged.

### Contract

- **When it runs**: on the first dispatch into the workspace, and again whenever
  `init.sh`'s content changes (compared by sha256 hash). The completion marker lives at
  `~/.local/share/boid/homes/<slug>.init.json`, outside `$HOME` itself, so a sandboxed job
  cannot tamper with it
- **Concurrent dispatch is serialized**: if multiple jobs dispatch into the same
  never-initialized workspace at once, `init.sh` still runs exactly once (an flock
  serializes it); the other callers wait for it to finish before continuing
- **Execution environment**: runs on the host (trusted) as `/bin/bash <script>` — the
  shebang line is ignored. The following environment variables are set:
  - `HOME` — the workspace home directory (subsequent installs should land here)
  - `BOID_WORKSPACE_SLUG` — the workspace's slug
  - `BOID_WORKSPACE_HOME` — same value as `HOME`
  - `PATH` / `USER` / `LOGNAME` / `LANG` / `LC_ALL` / `TERM` are inherited from the host
    unchanged. Every other host environment variable (including the host's own `HOME` /
    `XDG_*`) is deliberately NOT inherited
- **A failure fails dispatch**: a non-zero exit from `init.sh` does not silently skip
  initialization — the dispatch fails explicitly (the job ends up `failed`, the task
  `aborted`), and the error message includes the exit code and a tail of the script's
  output

### What script authors must guarantee

- **Idempotency**: the script must tolerate a corrupted completion marker or simply being
  re-run (always check "already installed? skip" yourself)
- **No interactive steps**: interactive auth flows (e.g. `claude login`) cannot run inside
  `init.sh`. Do those via the first-login flow below instead
- Everything else is up to you — installing toolchains (claude CLI / go / volta / codex /
  opencode / ...), dropping config files, etc. boid does not care what the script does

### Example

```bash
#!/bin/bash
set -euo pipefail

# Install the claude CLI (idempotent: skip if already present)
if ! command -v claude &>/dev/null; then
  curl -fsSL https://claude.ai/install.sh | bash
fi
```

Installing more toolchains (go, node via volta, codex, opencode, ...) just means repeating
the same pattern — "already installed? skip" — once per tool.

#### Copying non-embedded skills

boid's built-in skills (`/boid-task` etc.) are synced into the workspace home
automatically on every dispatch, so `init.sh` never needs to handle them.

Host-only custom skills you keep under `~/.claude/skills/<name>/` (e.g. a bitbucket or
jira skill) are a different story: `init.sh` cannot copy them, because although it runs on
the host, it is not given any variable pointing at the *real* host `$HOME` — `HOME` inside
`init.sh` is already the workspace home.

To use this kind of skill in a workspace, copy it by hand once, as a human, when you set
the workspace up:

```bash
mkdir -p ~/.local/share/boid/homes/<slug>/.claude/skills
cp -r ~/.claude/skills/bitbucket ~/.local/share/boid/homes/<slug>/.claude/skills/
```

## First login

`init.sh` only installs tooling. Logging into claude / codex / opencode requires an
interactive flow that cannot run inside `init.sh`.

With the workspace home freshly initialized (right after `init.sh` has run, say), start one
interactive session to log in:

```bash
boid agent claude -p <project-ref>
```

Go through the harness's normal login flow (browser-based auth, etc.) inside that session.
The credentials get written to that session's `$HOME` — i.e. the workspace home — so every
later job in that workspace stays authenticated.

**The host's `~/.claude.json` is never copied.** Logging in from a clean slate, per
workspace, is the intended contract — workspaces deliberately do not share host-side auth
state with each other.

## Removing a workspace

`boid workspace remove <slug>` removes both the workspace's definition (DB row) and its
home directory.

```
$ boid workspace remove my-workspace
home size: 128.4 MB (/home/you/.local/share/boid/homes/my-workspace)
workspace remove "my-workspace" — really delete it? [y/N]: y
workspace "my-workspace" removed (any assigned projects were re-assigned to "default").
home dir deleted: /home/you/.local/share/boid/homes/my-workspace (128.4 MB)
```

- **Confirmation prompt**: always shown regardless of whether a home directory exists or
  what size it reports (`--force` is the only way to skip it; `--yes` is an alias for
  `--force`)
- **Size shown**: apparent size (roughly `du --apparent-size` — the sum of each file's
  logical byte length, not block-based disk usage) — a rough indicator, not an exact
  figure
- **The `default` workspace cannot be removed**: it is the reserved fallback every project
  ends up re-assigned to, so it is protected outright

## `boid gc`'s workspace home listing

`boid gc` (and `boid gc --dry-run`) prints every workspace home directory it finds under
`~/.local/share/boid/homes/`, with its size:

```
$ boid gc
deleted: 3 tasks, 5 jobs, 5 actions, 2 runtimes, 0 sandbox tmp entries
workspace homes:
  my-workspace:            128.4 MB
  (orphan) old-workspace:  4.1 MB
  total:                   132.5 MB
```

- **Display only**: `boid gc` never auto-deletes a workspace home (unlike `runtimes/`,
  workspace homes are designed to be persistent data)
- **The `(orphan)` flag**: means only the home directory remains — the corresponding
  workspace (DB row / `workspace.yaml`) no longer exists. Typically the result of the
  workspace definition being removed some other way than `workspace remove` (e.g. deleted
  by hand)
- To actually clean up an orphan, run `boid workspace remove <slug>` by hand, or (if the
  workspace definition is already gone) delete
  `~/.local/share/boid/homes/<slug>/` directly
- A size that fails to compute is shown as `?` and excluded from the total (this is not
  treated as an error — `gc` still completes)

## See also

- Full design and contract: [`docs/plans/home-workspace-volume.md`](../../plans/home-workspace-volume.md) (Japanese)
- Parent design doc: [`docs/plans/container-based-boid.md`](../../plans/container-based-boid.md) (Japanese)
- Workspace CLI reference: [`docs/en/reference/cli.md`](../reference/cli.md)
