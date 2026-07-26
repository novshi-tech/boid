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

A workspace with no `init.sh` is treated as "no SCRIPT to run". The preparation step (prep)
described below still runs exactly once either way.

### Contract

- **When it runs**: on the first dispatch into the workspace, again whenever `init.sh`'s
  content changes (compared by sha256 hash), again whenever the workspace home itself
  has been emptied or replaced since it was last initialized (see the next bullet), and
  again whenever boid changes the environment it runs `init.sh` IN (see "When the init
  environment changes" below). The
  completion marker lives at `~/.local/share/boid/homes-meta/<slug>.init.json` — in the
  **daemon's own data directory** (the one holding `boid.db`), not in the workspace home
  — so it is outside the directory a sandboxed job gets mounted as its `$HOME`, and a job
  cannot reach it that way
- **The marker is checked against the home, not trusted on its own**: every successful
  init also writes a random identity token to `<workspace home>/.boid-workspace-home-id`
  and records the same value in the marker. Initialization is skipped only when both the
  script hash and that token match. So if the home directory is wiped while the marker
  survives — its parent directory is not guaranteed to be as durable as the daemon's own
  data directory — the next dispatch re-runs `init.sh` rather than handing the agent an
  empty `$HOME`. What this comparison protects against is the home being lost or replaced
  by accident (the directory disappearing, a volume being removed, a restore from an old
  backup) — not deliberate tampering by a job: a job can read this file inside its own
  `$HOME`, so it can wipe the home and write the recorded value back. That is structural
  (the home is the job's own writable area) and its worst outcome is that the next job in
  the same workspace runs against a home this one already broke, which a job could do
  regardless. Deleting, corrupting or replacing this file (with a symlink, a FIFO,
  anything) only ever costs one extra — harmless, since `init.sh` must be idempotent —
  initialization run, and can never take boid itself down: boid never follows this path
  as a symlink, and checks that it is a regular file of at most 1 KiB before reading it
- **When the init environment changes**: the completion marker also records a generation
  number identifying the environment that prepared the home. When a boid upgrade changes
  where `init.sh` runs (most recently: from the daemon process on the host to a throwaway
  container, which also moved `$HOME` to the path a job sees), the marker's generation no
  longer matches and `init.sh` runs once more for that workspace. The toolchain itself is
  already in the home, so an idempotent `init.sh` finishes quickly; what the re-run is
  really for is the artifacts an installer bakes an ABSOLUTE PATH into — symlink targets,
  shebangs, wrapper scripts — which have to be re-pointed at the new one. A plain version
  bump does not trigger this; only a change of environment does
- **Concurrent dispatch is serialized**: if multiple jobs dispatch into the same
  never-initialized workspace at once, `init.sh` still runs exactly once. Within one
  daemon process an flock serializes it; ACROSS processes the exclusion is a deterministic
  container name (`boid-ws-init-<installID8>-<slug>`), because an flock is released when
  its process dies while the init container outlives it. If an init container is still
  running under that name, the next dispatch waits for it to finish rather than racing it
- **Execution environment**: runs inside a **throwaway container boid starts** (trusted),
  with `bash`. It is no longer executed on the host.
  - The image is the **default boid runner image**. A workspace's own
    `container_image` override is NOT used: an override is only accepted if it carries
    the `boid.runner_protocol` label, and no image bakes that label yet — so honoring it
    would leave every workspace that sets `container_image` unable to prepare its home
    at all
  - The workspace home is mounted into that container, and `$HOME` is that **mount
    target** — the same path a job sees its own `$HOME` at. Tools that bake absolute
    paths under `$HOME` into wrapper scripts, shebangs or symlinks therefore keep working
    inside the sandbox
  - The network is the **engine's default bridge**, not the per-workspace network job
    containers get (`internal: true`, egress only through an allowlist proxy), so
    toolchain downloads succeed. The flip side is that **init.sh has unrestricted
    egress** — deliberately, as part of the trusted boundary: boid chooses the script and
    starts the container, and none of this runs inside a sandboxed job
  - The shebang line is ignored. **boid does NOT exec `init.sh` from its configured
    path**: the hashed bytes are written to a temporary file inside the container and
    bash is invoked on that copy, which is what guarantees that the hash recorded in the
    marker matches the bytes that actually ran. As a result:
    - `$0` is that container-local temp path (`/tmp/...`), not the original `init.sh`
      path. A `dirname "$0"` cannot be used to reach sibling files under
      `~/.config/boid/workspaces/<slug>/`
    - Do not depend on the script's own location (`source ./foo`, `$PWD`-relative reads,
      etc.). Inline whatever the script needs, or read from files already present in the
      workspace home
    - The temp file is removed after the run
  - `cwd` is set to `$BOID_WORKSPACE_HOME` (the workspace home)
  - **stdin is `/dev/null`** — reading from standard input yields nothing
  - Exactly **three** environment variables are set:
    - `HOME` — the workspace home (subsequent installs should land here)
    - `BOID_WORKSPACE_SLUG` — the workspace's slug
    - `BOID_WORKSPACE_HOME` — same value as `HOME`

    `PATH` comes from the image. The host's `PATH` / `USER` / `LOGNAME` / `LANG` /
    `LC_ALL` / `TERM` are **no longer forwarded** (they were, before this changed); set
    them yourself if you need them. Commands installed only on the host are unreachable —
    install anything the image lacks from within `init.sh`
- **The preparation step (prep) always runs**: at the start and end of that same
  container, boid creates the bind-target skeleton (`~/.claude`, and
  `~/.claude/skills/<name>` for each built-in skill) and writes the identity token
  described above. A workspace with no `init.sh` still gets exactly one container run
- **A failure fails dispatch**: a non-zero exit from `init.sh` does not silently skip
  initialization — the dispatch fails explicitly (the job ends up `failed`, the task
  `aborted`), and the error message names **which stage failed** (`prelude` /
  `script-setup` / `init.sh` / `postlude`), the exit code, and a tail of the output.
  `init.sh`'s own exit code is propagated unchanged

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

**Reference implementation**: [`docs/examples/workspace-home-init.sh`](../../examples/workspace-home-init.sh)
installs go / volta / node (lts) / claude / codex / opencode end-to-end
(`GO_VERSION` etc. overridable via env vars, RETURN trap for temp cleanup,
`command -v`-based idempotency checks). Copy it into
`~/.config/boid/workspaces/<slug>/init.sh` as a starting template and customize.

#### Copying non-embedded skills

boid's built-in skills (`/boid-task` etc.) are extracted from the boid binary on
every dispatch and **bind-mounted read-only, one mount per skill**, at
`~/.claude/skills/<name>`, so `init.sh` never needs to handle them (no copy of them
is stored inside the workspace home). They are mounted per skill rather than as one
mount of the whole directory so that the hand-copied skills below stay visible.

Host-only custom skills you keep under `~/.claude/skills/<name>/` (e.g. a bitbucket or
jira skill) are a different story: `init.sh` cannot copy them, because it runs inside a
throwaway container that has no access to the host filesystem at all — the workspace home
is the only thing mounted into it.

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
- **The `(orphan)` flag**: means only the home directory remains — **no matching DB
  workspace row exists** (`workspace.yaml`'s presence is not part of orphan detection).
  Typically the result of an old workspace whose DB row was removed but whose home
  directory was not cleaned up
- To actually clean up an orphan, **delete the files directly by hand**:
  ```bash
  rm -rf ~/.local/share/boid/homes/<slug>/
  rm -f ~/.local/share/boid/homes-meta/<slug>.init.json
  rm -f ~/.local/share/boid/homes-meta/<slug>.lock
  ```
  (An installation upgraded from an older boid may still have
  `~/.local/share/boid/homes/<slug>.init.json` / `.lock` lying around. The current daemon
  never reads those, so removing them is optional.)
  `boid workspace remove <slug>` cannot be used here — by orphan's definition the DB
  row is already gone, so the command would 404. Manual `rm` is the only cleanup path
- A size that fails to compute is shown as `?` and excluded from the total (this is not
  treated as an error — `gc` still completes)

## See also

- Full design and contract: [`docs/plans/home-workspace-volume.md`](../../plans/home-workspace-volume.md) (Japanese)
- Parent design doc: [`docs/plans/container-based-boid.md`](../../plans/container-based-boid.md) (Japanese)
- Workspace CLI reference: [`docs/en/reference/cli.md`](../reference/cli.md)
