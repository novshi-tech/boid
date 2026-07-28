# Workspace home setup guide

How to set up a workspace's persistent `$HOME` (workspace home) and do its first login.
See [`docs/plans/home-workspace-volume.md`](../../plans/home-workspace-volume.md) for the
full design background (Japanese only for now).

## What is a workspace home

Every workspace has a dedicated, persistent **docker named volume** called
`boid-ws-home-<installID8>-<slug>`. Every job (hook, exec, or session) that belongs to a
project assigned to that workspace mounts this volume as its sandbox `$HOME`, read-write.

> **Changed on 2026-07-27**: this used to be a host directory,
> `~/.local/share/boid/homes/<slug>/`. Under the container backend that path resolves
> below `$XDG_RUNTIME_DIR` — normally tmpfs — so **a host reboot took the credentials and
> the toolchain with it**. Moving to a named volume is what fixes that. The old
> directories are left in place, not deleted (a migration CLI has to read them).

- **Persists across jobs**: files written to `$HOME` by one job (credentials, package
  caches, installed tools, ...) are still there for the next job in the same workspace
- **`$HOME/.boid` is not an exception**: it persists like the rest of `$HOME`. Before
  Phase 6 PR8 it alone got a fresh, job-scoped tmpfs layered on top, but the file path
  that isolation existed for (`$HOME/.boid/output/payload_patch.json`) was retired, and
  the overlay went with it — see
  [`docs/en/reference/hook-contract.md`](../reference/hook-contract.md)
- **The only thing layered over the volume is the embedded-skill binds**: each embedded
  skill is bind-mounted read-only at `~/.claude/skills/<name>`. Their contents are not
  stored in the volume (the daemon unpacks them from the boid binary on every dispatch);
  every other path shows the volume itself
- **Not shared across workspaces**: workspace A's `$HOME` and workspace B's `$HOME` are
  different volumes, and neither is shared with the real host `$HOME` (nor with the
  `boid` daemon process's own `$HOME`)
- **To look inside one**: `docker volume inspect boid-ws-home-<installID8>-<slug>` reports
  the `Mountpoint`. Under rootless podman its contents are owned by a host subuid, so
  reading or writing them needs `podman unshare`

A project with no explicit workspace assignment uses the `default` workspace's home.

## Writing an init.sh

Placing an `init.sh` under a workspace's config makes it run automatically **on the first
dispatch into that workspace** — useful for one-time setup work in the workspace home,
such as installing the claude CLI.

### Location, and how to edit it

The `init.sh` belongs to the **daemon**. It is stored in the daemon's config directory
(`~/.config/boid/workspaces/<slug>/init.sh`, or `$XDG_CONFIG_HOME`'s equivalent), but that
path is on the **daemon's own filesystem**: under the container deployment it resolves
inside the daemon's state volume, so **editing `~/.config/boid/` on this host changes
nothing**.

Edit it through the CLI:

```bash
# print the current script (-o <file> writes it to a file instead)
boid workspace get-init-script <slug>

# upload a local file ("-" reads from stdin)
boid workspace set-init-script <slug> -f init.sh

# edit in $EDITOR, applied on save
boid workspace edit-init-script <slug>

# delete it, returning the workspace to the no-script state
boid workspace unset-init-script <slug>
```

`set-init-script` / `edit-init-script` / `unset-init-script` fetch the current revision
(ETag) first and send it back as `If-Match`, so a script that changed through another route
in between is reported rather than silently overwritten (`--force` skips the check).

`boid workspace export` includes the script as `spec.init_script`, and
`boid workspace apply -f <file>` restores it — that is how a workspace is moved to another
install. An explicit `spec.init_script: ""` means "this workspace has no init.sh" and
DELETES the target's on apply; omitting the key entirely leaves it untouched.

An empty script cannot be stored: writing empty content clears the script instead. An empty
script and no script are the same thing to dispatch, so boid keeps only one representation
of it.

An `init.sh` is capped at **128 KiB**, on every route that stores one — the dedicated
endpoint and `spec.init_script` in an applied document alike. A cap is needed at all because
the daemon buffers the whole script to hash it and wrap it in a heredoc; the particular
figure is derived from the guarantee that anything this API accepts can be restored through
`workspace export` → `workspace apply` — an export embeds the script in a yaml document,
which inflates it, and `apply` reads that document under a 1 MiB body cap. A hand-authored
init.sh is a few KB (the reference implementation is under 5 KB), so the cap is not a
practical constraint.

The other half of that guarantee is on the export side: if a workspace's exported document
would come out over the 1 MiB `apply` reads it under, **`boid workspace export` fails**
naming that workspace, rather than writing a file that cannot be restored. The script's
share is bounded by the cap above, so reaching this means the workspace's *metadata* is
enormous — an `env` value of several hundred KB or similar. Shrink it, or drop `--all` and
export the remaining workspaces one at a time with `boid workspace export <slug>`.

An export also fails on a **daemon version skew**: if a document in the response carries no
`spec.init_script` — that daemon predates the field and knows nothing about init.sh — the
CLI names the workspace and writes **no file**. That document is valid yaml and would apply
cleanly, but it restores the metadata alone, leaving a workspace with **no init.sh** and so
no harness on its next dispatch. Upgrade the daemon and export again. `boid workspace apply`
still accepts a document without `spec.init_script`, on purpose: a hand-written document
that only adjusts `env` should not have to carry a copy of the whole script, and there an
absent key correctly means "leave it alone".

Driving the API directly, the body's `Content-Type` may be **omitted**. The only types
refused are structured-data formats — yaml, json, xml, tar and the like — whose arrival
means the request is at the wrong endpoint (a workspace yaml document goes to
`PUT /api/workspaces/{slug}`, a boid.dev/v1 envelope to `POST /api/workspaces/apply`).

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
- **The marker is checked against the home, not trusted on its own**: the home volume is
  created carrying a random identity token on its `boid.workspace_home_id` label, and the
  marker records the same value. Initialization is skipped only when both the script hash
  and that token match, so if the volume is removed while the marker survives, the next
  dispatch re-runs `init.sh` rather than handing the agent an empty `$HOME`. What this
  detects is the **volume being deleted and re-created** (`docker volume rm`, a reap
  misfire, a half-completed workspace removal). What it does **not** detect is the volume
  surviving while its contents are emptied or replaced in place — nothing on the daemon
  side can see inside a volume (that would take starting a container, and doing so on
  every dispatch would defeat the point of having a completion marker at all), so the
  check is limited to what the daemon can ask the engine.
  Deliberate tampering by a job was never covered either, before or after: the home is the
  job's own writable area, so this is structural, and its worst outcome is that the next
  job in the same workspace runs against a home this one already broke — which a job could
  do regardless.
  If boid finds a `boid-ws-home-*` volume it did not create (no identity label — somebody
  made it by hand), it **fails the dispatch and says so**: the Engine API cannot add a
  label to an existing volume, so re-initializing would never converge
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
    `container_image` override is NOT used here: the prep only needs `bash`, coreutils
    and whatever the operator's `init.sh` reaches for, and honoring an override would
    put that override's own failure modes (an unpullable image, a stale
    `boid.runner_protocol` label) on the path that prepares the home — leaving the
    workspace unable to prepare its home at all, rather than unable to run one job
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
- **The preparation step (prep) always runs**: ahead of your script, in that same
  container, boid creates the bind-target skeleton (`~/.claude`, `~/.claude/skills`, and
  `~/.claude/skills/<name>` for each built-in skill). A workspace with no `init.sh` still
  gets exactly one container run. The identity token described above is not written here
  — it is stamped on the volume itself when the volume is created, so nothing inside the
  home carries it
- **A failure fails dispatch**: a non-zero exit from `init.sh` does not silently skip
  initialization — the dispatch fails explicitly (the job ends up `failed`, the task
  `aborted`), and the error message names **which stage failed** (`prelude` /
  `script-setup` / `init.sh`), the exit code, and a tail of the output.
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
`command -v`-based idempotency checks). Customize it as a starting template, then register
it with `boid workspace set-init-script <slug> -f docs/examples/workspace-home-init.sh`.

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
# The workspace home is a named volume, so doing this from init.sh is the reliable
# route. From the host, go through the volume's Mountpoint:
VOL=$(docker volume inspect -f '{{.Mountpoint}}' boid-ws-home-<installID8>-<slug>)
mkdir -p "$VOL/.claude/skills"
cp -r ~/.claude/skills/bitbucket "$VOL/.claude/skills/"
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

## Migrating a legacy (host-directory) workspace home

Installations that predate the move to named volumes (2026-07-27) still have each
workspace's credentials and toolchain sitting in `~/.local/share/boid/homes/<slug>/`. The
volume side starts out empty, so moving the contents across takes one run of the migration
command:

```bash
boid workspace import-home <slug>                        # from ~/.local/share/boid/homes/<slug>
boid workspace import-home <slug> --from /path/to/backup # restoring from a backup
boid workspace import-home <slug> --dry-run              # just show what would move
boid workspace import-home <slug> --yes                  # skip the confirmation (--force is the same)
```

```
$ boid workspace import-home khi --dry-run
dry-run: would import /home/nosen/.local/share/boid/homes/khi into workspace "khi"'s HOME volume
  38412 files, 4102 dirs, 517 symlinks (1204 hard links), 4.3 GB
  nothing was sent and nothing was destroyed; re-run without --dry-run to migrate
```

### How it works

- The CLI **reads** the `--from` directory, packs it into a tar and **streams it to the
  daemon**. The daemon replaces the volume and feeds the tar to a throwaway container.
- **No bind mount is involved.** Under rootless podman a container's uid is not the host's,
  so a bind makes a `0600` `.credentials.json` unreadable — the same trap that triggered
  the volume-only pivot. A tar crosses the mapping as data: the host user reads their own
  files, and the extraction re-creates them as the container's uid.
- **Modes are preserved** (`0600` stays `0600`). Ownership is not carried across; everything
  lands owned by the same uid a job container runs as.
- **Hard links stay links** (node/volta toolchains use them heavily, so the payload is not
  sent twice).
- Sockets, FIFOs and device nodes cannot be restored, so they are **skipped and listed by
  name**.
- **A `--from` that is itself a symlink is followed** (`homes/team-a -> /mnt/backup/team-a`
  — the ordinary way to point at a home that lives on another disk). Where it resolved to is
  printed in the confirmation prompt and in `--dry-run` output as `typed (-> resolved)`, so
  what is about to be read is visible before anything happens. **Symlinks INSIDE the home are
  not followed** — those are the home's own structure and cross as symlinks (above).

### Safety

- **`--from` is only ever read.** Nothing deletes or modifies it, so the migration can be
  re-run as often as needed.
- **A workspace with a running job is refused.** The engine answers 409 for a volume a live
  container is holding, and at that point **nothing has been destroyed yet**:
  ```
  Error: import workspace home: workspace home "khi": refusing to migrate while a job is
  running in it — the engine is holding the volume "boid-ws-home-a1b2c3d4-khi" for a live
  container, and nothing has been changed. Wait for the job to finish (`boid job list`) and re-run
  ```
- **A job that is merely STARTING is refused too.** The engine only considers a volume in use
  once a container references it, so the interval between the daemon resolving the home volume
  and creating the container looks free from the engine's side. A migration slipping through
  there would be extracting into the very volume a starting job is about to mount — job
  containers mount it by NAME. The daemon tracks that interval itself and answers the same 409
  (`the workspace HOME volume is busy in this daemon`), also before anything is destroyed.
- **A dispatch that arrives DURING a migration waits rather than failing.** A migration is
  something a person runs at a terminal, so refusing it costs one re-run; a dispatch comes out
  of hook evaluation, and refusing that means a failed job recorded on the task. The wait is
  bounded by how long the migration takes.
- **A source with nothing to migrate is refused.** Sending an empty tar would, because of the
  order this route cannot avoid (the volume is destroyed before the body is read), **replace
  the home with an empty one and answer 200** — the credentials and the toolchain gone while
  the CLI prints "imported". That is the worst outcome this feature can produce, so it is
  refused on **both** sides: by the CLI before a single byte is sent and before the
  confirmation prompt, and by the daemon before anything is destroyed. The causes vary (a typo
  in `--from`, a backup that was never populated, a mount that is not mounted) and the outcome
  does not, so the check is on the outcome. To deliberately start a workspace's home over, use
  `boid workspace remove <slug>` and re-create it.
- **The existing home volume is destroyed.** There is no undo, and the confirmation prompt
  appears unless `--yes` is given.
- **If the transfer fails partway**, the workspace ends up in the same state as one that was
  never dispatched into: the half-extracted volume is **removed** and the completion marker is
  gone, so the next dispatch rebuilds it from `init.sh` starting from nothing. The source is
  untouched, so simply re-running the migration is the recovery. The volume is removed rather
  than left in place because a partial home is what makes an idempotent `init.sh` — the kind
  that probes with `command -v claude` and returns early — conclude the toolchain is already
  installed: it would exit 0, the daemon would write a completion marker for it, and the
  broken state would be frozen in (a delivered `.local/bin/claude` passes that probe even when
  its payload never crossed).
- **If that removal ALSO fails**, it is reported alongside the extraction error. Removing the
  volume by hand as the message instructs (`docker volume rm <volume>`) and re-running is the
  quickest repair, but leaving it alone is also safe: the next daemon start picks it up.
- **If the daemon is killed mid-migration** (SIGKILL, an OOM kill, `docker compose down -t 0`,
  a power cut) the half-extracted volume does not survive either. Before it destroys anything
  the daemon writes an in-progress record to `<dataHome>/homes-meta/<slug>.migrating.json`, and
  it keeps that record's `phase` field current as it goes:

  | `phase` | written when | what the next start (or dispatch) does |
  |---|---|---|
  | `recorded` | before anything is destroyed | deletes the record, touches nothing else |
  | `home-destroyed` | once the old volume is actually gone | deletes the home volume, the completion marker and the record |
  | `home-absent` | once there is confirmed to be no volume under that name | deletes the record, touches nothing else |
  | `home-rebuilt` | once the extraction has finished | deletes the record, touches nothing else |

  Because `home-destroyed` is written before the replacement volume is created and replaced
  only after the extraction finishes, **a kill at any point falls on the safe side**: a kill
  while the volume genuinely holds a half-written home discards it and rebuilds from `init.sh`,
  and a kill at any other moment leaves the home alone. A record with no `phase` at all (one
  written by a boid older than this) is read as `home-destroyed`, because that is what those
  records meant.

  `home-absent` covers a migration that ended early with the volume confirmed gone — an
  extraction that failed and whose half-written volume was then successfully removed, for
  instance. It is written **before** the record is deleted, and that ordering is the whole
  point: deleting the record fails in the awkward direction (it looks deleted, and a power cut
  can bring it back), so relying on the deletion and leaving the phase alone would let a later
  start discard a home that had been rebuilt correctly in the meantime.
- **If the startup cleanup fails, the next dispatch picks it up.** An engine that is briefly
  unreachable at boot makes that volume removal fail; the record stays and startup continues.
  A dispatch into a workspace that still has a `home-destroyed` record retries the removal
  **before it runs `init.sh`**, and rebuilds from an empty home once it succeeds — so a
  transient engine problem heals itself. If the removal still fails, that dispatch **fails**:
  losing one job is cheaper than running `init.sh` over a partial home and freezing the
  breakage behind a fresh completion marker. The error names the volume and the
  `docker volume rm` that clears it. Each such removal is bounded (30s), so an engine that
  accepts the connection and then answers nothing cannot park daemon startup.
- **If the record cannot be made durable, the migration stops with nothing destroyed.** The
  record is written immediately before the first destructive step precisely so this failure is
  free — check that the daemon's state area is writable and re-run. The same applies to the
  `home-destroyed` update: if the old volume was removed but that fact could not be recorded
  durably, the migration stops there rather than extract into a volume nothing could later
  identify as unfinished. The home is then simply empty, and the next dispatch rebuilds it.
  A `home-absent` update that cannot be made durable is different: the record is **kept**
  rather than deleted, because on a state area that cannot be flushed the deletion would fail
  in the looks-deleted-but-comes-back direction. The record that stays is then resolved
  harmlessly by the next start or dispatch, against a volume that no longer exists.
- **If only the record's deletion fails after a successful migration**, that is reported as a
  warning naming the file. In the ordinary case the record was already moved to `home-rebuilt`,
  so **the home is safe** and the next daemon start just deletes the file — nothing to do,
  though an unwritable state area is worth looking into. Only if the warning also says the
  **next daemon start will discard this home** does the record still say `home-destroyed`; then
  delete the named file before restarting.

### init.sh runs once after the migration

That is intended. A migration copies CONTENTS, and none of the five things boid's completion
marker records (script hash, generation, skeleton set, volume identity) changes as a result.
Left alone, a toolchain with host-era absolute paths baked into it would survive forever and
`init.sh` would never run again. So the migration does **both** of the available things:

1. **Replaces the volume** — a new incarnation carries a new identity, which no longer
   matches the marker's `home_id`.
2. **Deletes the completion marker** — `<dataHome>/homes-meta/<slug>.init.json`.

Either one alone is enough to force the re-init, which is why both are done: whichever step
fails, the result still falls on the re-initialize side. Since the home is already populated,
an idempotent `init.sh` finishes quickly.

## Migrating WITHOUT `import-home` — removing the marker by hand

If you replace a volume's contents by any other means — `docker cp`ing files in, editing the
volume's Mountpoint through `podman unshare`, restoring the whole volume from a backup —
**boid cannot detect it**.

That is the boundary itself rather than a gap in the implementation. The daemon cannot see
INSIDE a volume (doing so needs a container, and starting one on every dispatch would defeat
the point of having a completion marker at all). What the marker identifies is the
**incarnation of the container**, not its contents, so "same volume, different contents" is
invisible by construction.

So **whoever performed that operation has to delete the completion marker by hand**. Without
that, `init.sh` never runs again and whatever was copied in is never initialized.

**The marker cannot be deleted from the host directly.** It lives in the daemon's own
persistent root (`<dataHome>/homes-meta/<slug>.init.json`), which under the container deploy
is inside the `boid_state` named volume — there is no host path for it. Per deployment shape:

```bash
# (a) container deploy (docker compose / scripts/deploy-container.sh) — through the daemon
docker exec boid_daemon_1 rm -f /home/boid/.local/share/boid/homes-meta/<slug>.init.json

# (b) daemon stopped, or exec unavailable — reach the volume from a throwaway container
docker run --rm -v boid_boid_state:/state docker.io/library/debian:bookworm-slim \
  rm -f /state/.local/share/boid/homes-meta/<slug>.init.json

# (c) bare `boid start` (daemon as a host process) — an ordinary file
rm -f ~/.local/share/boid/homes-meta/<slug>.init.json
```

- The volume name (`boid_boid_state`) is the compose project name (`boid`) plus the volume
  name; `docker volume ls` confirms it.
- The daemon-side path under the container deploy is `XDG_DATA_HOME=/home/boid/.local/share`
  (set in compose) plus `boid/homes-meta/`.
- **Removing the whole volume is the more reliable route**: `docker volume rm
  boid-ws-home-<installID8>-<slug>` followed by `boid workspace import-home`. That changes
  the identity too, so there is no marker left to forget.
- Deleting the marker only loses its `boid_version` / `completed_at` history. With no marker,
  the next dispatch re-initializes (init scripts are contractually idempotent).

## Removing a workspace

`boid workspace remove <slug>` removes both the workspace's definition (DB row) and its
home.

```
$ boid workspace remove my-workspace
home size: 128.4 MB (volume boid-ws-home-a1b2c3d4-my-workspace)
workspace remove "my-workspace" — really delete it? [y/N]: y
workspace "my-workspace" removed (any assigned projects were re-assigned to "default").
home volume deleted (volume boid-ws-home-a1b2c3d4-my-workspace) (128.4 MB)
```

- **Confirmation prompt**: always shown regardless of whether a home exists or what size
  it reports (`--force` is the only way to skip it; `--yes` is an alias for `--force`)
- **The identifier shown is the volume name**: the workspace home is a named volume, so
  what is printed is exactly what you would pass to `docker volume rm`
- **Size shown**: the volume size the engine reports through `docker system df`, which
  has `du --apparent-size` semantics (sparse files count logically, a hardlinked file
  counts once, a symlink counts its target string's length) — a rough indicator, not an
  exact block-based figure
- **The `default` workspace cannot be removed**: it is the reserved fallback every project
  ends up re-assigned to, so it is protected outright
- **The workspace's `init.sh` is deleted too**: dispatch resolves a script from the slug
  alone, so leaving it behind would make a **workspace re-created under the same name
  inherit the old script** and run it against a brand-new HOME volume. It is best-effort
  like the home volume — a failed deletion still returns success and reports the
  daemon-side path in a warning, and it cannot be cleaned up with
  `boid workspace unset-init-script` afterwards (the row is already gone), so it has to be
  removed by hand on the daemon
- **Deletion fails while a job is running in that workspace**: the engine refuses to
  remove a volume that is in use with a 409, and no force flag overrides that. The result
  is a **part-completed** remove — the DB row is gone, the volume is not — reported as:
  ```
  warning: home volume delete failed (volume boid-ws-home-a1b2c3d4-my-workspace): ...volume is being used by...
  ```
  Re-running `boid workspace remove` will 404, since the row is already gone. Once the job
  finishes, run `docker volume rm boid-ws-home-a1b2c3d4-my-workspace` by hand.
- **Removal is refused with a 409 while `boid workspace import-home` is running for that
  workspace**: a migration deletes the home volume, **re-creates it under the same name and
  writes into it**, so overlapping the two leaves the DB row gone and a volume full of harness
  credentials behind — one no dispatch can mount and `boid workspace remove` can no longer
  delete (it 404s on the missing row). The refusal lands **before the row is deleted**, so
  nothing has been changed; wait for the migration to finish and re-run.
  ```
  Error: workspace home "my-workspace": refusing to remove this workspace while
  `boid workspace import-home` is replacing its HOME volume ...
  ```
  The reverse order (a removal already in flight when a migration starts) is refused the same
  way.
- **A deletion that could not be confirmed is reported, not passed over in silence**: if
  the daemon does not say whether the home volume went away — because it could not reach
  the engine to check, or because it is older than the volume rewiring and still answers
  in terms of a host directory — the CLI says so and tells you how to look:
  ```
  warning: could not confirm the home volume was deleted: the daemon reported no home volume name (a daemon older than the volume rewiring, or one with no engine handle)
    the workspace row is gone either way; check with `docker volume ls --filter label=boid.workspace_home=my-workspace` and remove any match with `docker volume rm <name>`
  ```
  This matters most when your `boid` CLI and daemon are different versions, which is
  possible since the CLI can drive a remote daemon. The label filter works regardless of
  version because the volume carries the workspace slug verbatim as a label.
  A workspace that was never dispatched into simply has no home volume, and that case
  stays quiet — the daemon can tell the two apart, so the warning only appears when
  something really is unknown.

## `boid gc`'s workspace home listing

`boid gc` (and `boid gc --dry-run`) prints every workspace home it finds, with its size:

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
- **The `(orphan)` flag**: means only the home volume remains — **no matching DB
  workspace row exists** (`workspace.yaml`'s presence is not part of orphan detection).
  Typically the result of an old workspace whose DB row was removed but whose home volume
  was not cleaned up, or of the part-completed remove described above
- **Only this install's homes are listed**: volume names are
  `boid-ws-home-<installID8>-<slug>`, so two boid installs sharing one engine never list
  each other's homes. The flip side: a volume created before this install had an id
  (`boid-ws-home-noinst-<slug>`) is not listed either, because the current daemon no
  longer mounts it — find those with `docker volume ls` and remove them by hand
- To actually clean up an orphan, **delete the files directly by hand**:
  ```bash
  docker volume rm boid-ws-home-<installID8>-<slug>
  rm -f <dataHome>/homes-meta/<slug>.init.json
  rm -f <dataHome>/homes-meta/<slug>.lock
  ```
  (An installation upgraded from an older boid may still have
  `~/.local/share/boid/homes/<slug>.init.json` / `.lock` lying around. The current daemon
  never reads those, so removing them is optional.)
  `boid workspace remove <slug>` cannot be used here — by orphan's definition the DB
  row is already gone, so the command would 404. Manual `rm` is the only cleanup path
- **Two engine calls, two different degrades.** The listing and the sizes do not come
  from the same request — the volumes are enumerated (`docker volume ls` equivalent) and
  then sized in one engine-wide sweep (`docker system df` equivalent) — so failing to
  reach the engine degrades differently depending on which call it was:
  - **only the sizing call failed**: the listing is kept, and every entry shows `?`.
    A `?` size is excluded from the total (an unknown size must not silently understate
    it) and is not treated as an error — `gc` still completes. Because the sizes come
    from one sweep rather than one call per volume, this is normally all-or-nothing: a
    listing where every entry is `?` means that single call failed, not that the volumes
    are individually unreadable
  - **the enumeration itself failed**: there is no listing to show, so `boid gc` prints
    the reason instead of the table and still reports the deletions it actually
    performed:
    ```
    deleted: 3 tasks, 5 jobs, 5 actions, 2 runtimes, 0 sandbox tmp entries
    workspace homes: listing unavailable (list workspace home volumes: ...)
    ```
    The reason is printed rather than only logged on the daemon, because an omitted
    section would otherwise look exactly like an install that simply has no workspace
    home volumes yet

## See also

- Full design and contract: [`docs/plans/home-workspace-volume.md`](../../plans/home-workspace-volume.md) (Japanese)
- Parent design doc: [`docs/plans/container-based-boid.md`](../../plans/container-based-boid.md) (Japanese)
- Workspace CLI reference: [`docs/en/reference/cli.md`](../reference/cli.md)
