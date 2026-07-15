# `project.yaml` reference

Every field of `.boid/project.yaml`, the file that lives at the root of a `boid` project.

This page is the schema reference. For the meaning of the underlying terms, see [Concepts](../guide/concepts.md). For walkthroughs, see [Getting started](../getting-started/).

## Role and location

- Path: `.boid/project.yaml` directly under the project root.
- Role: registers the directory as a `boid` project, declares the kinds of tasks (behaviors) it supports, and the extension packages (kits) the project loads.
- Registration: `boid project add <project-root>` reads the file into `boid`'s database.
- Reload: after editing, run `boid project reload`.

## Minimal example

```yaml
id: demo
name: Demo
task_behaviors:
  supervisor:
    name: Supervisor
```

## Top-level fields

| Key | Type | Required | Role |
|---|---|---|---|
| `id` | string | yes | Unique identifier for this project inside `boid`. Tasks reference it via `project_id`. |
| `name` | string | yes | Display name shown in UIs. |
| `worktree` | bool | `false` | Used to allocate a dedicated **isolated branch** (`boid/<id8>`) to executor and supervisor tasks when `true`. **docs/plans/branch-policy-simplification.md Phase 1 (v0.0.11) retired the per-task branch and fork-point concepts**: every task, root or child, now checks out `base_branch` directly on its in-sandbox clone, so this field no longer affects checkout behaviour (it is still accepted for schema compatibility). See [Task kinds and HEAD branch](#task-kinds-and-head-branch) for the full breakdown. |
| `base_branch` | string | (see below) | The PR target branch, resolved at task creation and stored in the row. **When omitted**: root tasks expand to the daemon's current HEAD branch (`${current_branch}` equivalent) at creation time — a detached-HEAD repository returns 400. Child tasks inherit the parent's `base_branch`. Supports `${TASK_REMOTE_ID}` and `${current_branch}` expansion (see [Dynamic base_branch](#dynamic-base_branch)). |
| `fork_point` | string | (falls back to `origin/HEAD`) | Fork origin for case 3 (when `base_branch` does not yet exist locally or on `origin`). Any ref resolvable by `git rev-parse --verify` (branch / tag / SHA / `origin/main`). Falls back to `refs/remotes/origin/HEAD`; errors if neither is resolvable. |
| `kits` | list of KitRef | no | Kits loaded for this project. |
| `task_behaviors` | map (string → TaskBehavior) | yes | The kinds of tasks this project can produce. |
| `host_commands` | HostCommands | no | External commands the sandbox is allowed to forward to the host. |
| `additional_bindings` | list of BindMount | no | Extra paths to mount into the sandbox. |
| `env` | map (string → string) | no | Environment variables to set inside the sandbox. |
| `secret_namespace` | string | no | Namespace under which this project's secrets are resolved. |
| `capabilities` | Capabilities | no | Declares optional sandbox capabilities. The only supported capability today is `docker`. |
| `default_task_behavior` | string | no | The behavior to use when `boid task create` omits `--behavior`. When unset, the daemon falls back to `supervisor` if that behavior exists (with a deprecation warning); if neither is configured, `boid task create` returns an error. |

## git gateway / in-sandbox clone

Every project-visible job (whether a hook, a session, or `boid exec`) freshly clones the project into the sandbox through the **git gateway** — a credential-injecting reverse proxy hosted inside the daemon. The host-side project directory is never bind-mounted, and no host-side git worktree is created.

- The clone's origin points at the gateway. Inside the sandbox git is a plain, credential-less binary; the gateway is what proxies fetch/push to the upstream (GitHub etc.) with the token injected.
- **The only way results are shared is a push to origin**: uncommitted (or committed-but-unpushed) work is invisible to any other session or host. "Push before done" becomes a hard prerequisite.
- Under a `readonly: true` behavior, the local clone can still be written; the gateway rejects `git-receive-pack` (push) while leaving `git-upload-pack` (fetch) allowed. Read/write enforcement is now symmetric — "can't cross the boundary" instead of "can't touch the disk."
- `reopen` is realised as "re-clone + branch checkout"; only committed (+pushed) content is guaranteed to survive.
- Multiple tasks that target the same project × same HEAD branch dispatch **in parallel** now — each holds its own independent clone (the former per-branch FIFO lock is retired). Concurrent pushes resolve via the normal git flow (non-fast-forward reject → fetch + merge/rebase → re-push).
- A workspace peer project is fetch-only from inside the sandbox (clone / reference). To write to a peer, spawn a cross-project child task on that project.

## `task_behaviors.<name>`

The map key is the behavior's identifier and is what `boid task create` references via `behavior:`. The two **canonical** names are:

| Name | Role |
|---|---|
| `supervisor` | Readonly orchestrator. Reads the request, triages it, creates child executor tasks, monitors them. Never edits files. |
| `executor` | Writable implementer. Receives a single focused task and produces an artifact (commit / PR / payload trait). |

Any other map key is also accepted (Track A2 and later: `readonly` defaults to `true` as a fail-safe for non-canonical names; set `readonly: false` explicitly for writable behaviors). The legacy keys `plan` (alias for `supervisor`) and `dev` (alias for `executor`) are still accepted during the migration period but are deprecated.

Each behavior entry's fields:

| Key | Type | Default | Role |
|---|---|---|---|
| `readonly` | bool | `true` (fail-safe) | Whether the task's working directory is mounted read-only. `executor` retains `readonly: false` as a compatibility override (with a deprecation warning); all other behaviors default to `true`. Set `readonly: false` explicitly for any writable behavior. |
| `traits` | list of string | (empty) | Top-level payload trait names this behavior is allowed to use (e.g. `[artifact]`). |
| `default_instruction` | Instruction | (empty) | A single Instruction template appended to `Task.Instructions` when a task is created. |
| `kits` | list of KitRef | (empty) | Additional kits loaded only for this behavior, merged with the project-top `kits` list. |

> **Note:** a `name` field under `task_behaviors.<name>` is silently ignored by the loader. Use the map key as the behavior identifier.

### Removed behavior-level fields

The fields below used to live under `task_behaviors.<name>.*`. They have been moved to the project top level (or are now configurable at the behavior level with a different semantic).

| Field | Status / Location |
|---|---|
| `readonly` | Re-enabled at the behavior level in Track A2. Defaults to `true` (fail-safe); set `readonly: false` for writable behaviors. |
| `worktree` | Project-top `worktree:`. No longer affects checkout behaviour as of branch-policy-simplification Phase 1 — see the top-level fields table above. |
| `base_branch` | Project-top `base_branch:`. |
| `branch_prefix` | Not configurable. Child-task branches are always created under `boid/`. |
| `default_payload` | Removed. Provide payload at task creation time instead. |

### Dynamic `base_branch`

`base_branch` supports two interpolation tokens:

- `${TASK_REMOTE_ID}` — the remote identifier (e.g. a GitHub PR number) the parent supervisor recorded for this task. Resolved for both supervisor and executor. Used in the "1 Supervisor 1 PR" workflow ([Workflow 3](../../workflows.md#workflow-3--1-supervisor-1-pr)) to give each supervisor session its own integration branch.
- `${current_branch}` — resolved to the daemon's current HEAD branch in the project repository at task creation time.

**Resolution priority when omitted:**

1. **Child task** (`parent_id` is set): inherits the parent's `base_branch` verbatim. No template expansion is performed.
2. **Root task, base_branch omitted** (`parent_id` is empty): the value is expanded to `${current_branch}` and saved into the task row at creation time. If the repository is in detached-HEAD state, the request returns 400.
3. **Root task, base_branch provided**: template tokens (`${TASK_REMOTE_ID}` / `${current_branch}`) are expanded normally.

See [docs/workflows.md](../../workflows.md) for end-to-end examples (Workflow 3 is the canonical example of a dynamic supervisor `base_branch`).

For the history behind the `worktree` field, see [Concepts / Worktree](../guide/concepts.md#worktree).

### Task kinds and HEAD branch

**docs/plans/branch-policy-simplification.md Phase 1 (v0.0.11) retired the per-task branch (`boid/<id8>`) and fork-point concepts.** Regardless of task kind (root / child, supervisor / executor), the in-sandbox clone always checks out `task.BaseBranch` directly. The worktree-era mechanism — giving each child an isolated branch to fork from — is no longer needed now that the clone itself is the isolation unit: two clones can check out the same branch name without conflict.

| Task kind | HEAD branch | Read-only |
|---|---|---|
| **root sup / root exec** | `task.BaseBranch` | sup=true / exec=false |
| **child sup / child exec** | `task.BaseBranch` | sup=true / exec=false |

- **Root tasks** (`parent_id == ""`): the in-sandbox clone checks out `base_branch` directly (no new branch is created). If `base_branch` does not exist on origin yet (case 3), it is created locally from the resolved `fork_point` start point (see the [top-level fields table](#top-level-fields)).
- **Child tasks** (have a parent): handled identically to root tasks — `base_branch` is checked out directly. When `base_branch` is omitted, the child inherits the parent's `base_branch` verbatim (no template expansion — see [Dynamic base_branch](#dynamic-base_branch)), so parent and child check out the same branch name unless one is explicitly overridden.
- `task.BaseBranch` propagates to all child tasks as the PR target and is passed to executors via the `BOID_BASE_BRANCH` environment variable.

**Sibling executors that run in parallel against the same base_branch can collide on push.** This is not a regression — it is the same executor-side rebase/retry contract as before (see the next section). To truly isolate parallel children, have the supervisor assign each one a distinct `base_branch` (e.g. `feature/BGO-214`, `feature/BGO-215`, `feature/BGO-216`).

### Concurrent tasks on the same HEAD branch

Previously multiple tasks targeting `<projectID>:<HEAD branch>` were serialised by a FIFO lock (a single host git worktree could not host two tasks simultaneously). **That serialisation lock was retired by the git gateway cutover**: each task now holds an independent in-sandbox clone, so multiple tasks against the same branch dispatch in parallel. Concurrent pushes are resolved by the normal git flow — the second push gets a non-fast-forward reject, and the caller fetches + merges/rebases + re-pushes. If you want to avoid the collision entirely, assign distinct `base_branch` values per child as described above.

### Base synchronisation and merge responsibility

`boid` core does not control child task dispatch order or base synchronisation. A sub-supervisor orchestrates its children's dispatch order, but under the clone model there is no longer a sub-supervisor "own branch" to keep in sync — the sub-supervisor's own dispatch is also just a direct `base_branch` checkout:

```
A (executor) done → A's PR is merged into base_branch (on origin)
                         ↓
            sub-sup dispatches B → B's clone fetches fresh from origin,
                                    so it already includes A's merge
```

The merge command, timing, and target are the **responsibility of the project instruction**, not of any skill or boid core component. Core's only contribution is passing the `BOID_BASE_BRANCH` environment variable. (`BOID_PARENT_BRANCH` was removed in docs/plans/branch-policy-simplification.md Phase 1 — a grep across production project.yaml / e2e scripts found no real usage.)

### `default_instruction`

A single Instruction object. At task creation it is appended to `Task.Instructions` and becomes the active instruction the first time the task enters `executing`.

A `boid task reopen <id> --message "..."` call appends a new Instruction at the end of the array; the last element is what the agent sees, and `agent` / `model` are inherited from the previously active one.

## Shared building blocks

### KitRef

Each entry in a `kits` list is either:

- A string of the form `github.com/<owner>/<repo>/<sub-path>` (e.g. `github.com/novshi-tech/boid-kits/claude-code`), or
- A map:
  ```yaml
  kits:
    - ref: github.com/novshi-tech/boid-kits/claude-code
      as: agent
  ```
  `as` assigns an alias, useful when two kits would otherwise collide on agent name.

`<sub-path>` is optional — if the kit lives at the repository root, omit it.

### HostCommands

By default the sandbox cannot invoke commands on the host. `host_commands` declares an allow-list of what to forward. Two forms are supported.

List form (allow with no further restrictions):

```yaml
host_commands:
  - gh
  - aws
```

Map form (with per-command restrictions):

```yaml
host_commands:
  gh:
    allow: [pr, issue, run]
    deny: ["* delete*"]
    env:
      GH_REPO: ${boid:repo_slug}
    reject:
      - match: "*--body-file*"
        reason: 'Sandbox file paths are not visible on the host. Use --body "$(cat <file>)" instead.'
  aws:
    path: /usr/local/bin/aws
    env:
      AWS_REGION: ap-northeast-1
```

Each entry (a `HostCommandSpec`) has:

| Key | Type | Role |
|---|---|---|
| `allow` | list of string | Allowed subcommands or glob patterns (entries containing `*` or `?` are treated as patterns). |
| `deny` | list of string | Patterns that override `allow`. |
| `reject` | list of RejectRule | Rejects invocations whose args match the glob in `match`, with a mandatory `reason`. The rejection surfaces to the agent as `host_commands.<name>: rejected: <reason>` (see "Host command execution contract" below). |
| `stdin` | bool | **Deprecated.** Still parsed but always ignored (see "Host command execution contract" below). |
| `path` | string | Absolute path of the binary, overriding `$PATH` lookup. |
| `env` | map (string → string) | Extra environment variables to set when invoking this command. A value of `${boid:repo_slug}` expands to a context variable (see "Host command execution contract" below). |

A specialised use: setting `path` to a relative path inside the project or a kit forwards only that exact path to the host (for example `path: e2e/run.sh`).

> **Reserved names:** `git`, `boid`, and `fetch` are built-in sandbox commands. Declaring them in `host_commands` has no effect — they are always routed internally.

#### Host command execution contract

- **stdin never reaches the command** — the sandbox shim does not read stdin, and the broker discards anything it receives. `stdin: true` is still accepted (parsed) but has no effect (a deprecation warning is logged). To pass file contents or long text to a command, use an argument instead of stdin (e.g. `--body "$(cat <file>)"`).
- **cwd is a fixed neutral directory** — host commands run on the host in a neutral directory (`os.TempDir()`), never in any project checkout. Do not rely on behavior that infers the repo from cwd (e.g. `gh`'s implicit `-R`).
- **repo context is passed via env** — instead of cwd inference, an `env:` value of `${boid:repo_slug}` expands to a `host/owner/repo` string derived from the project's origin remote at token registration time. For `gh`, `GH_REPO: ${boid:repo_slug}` keeps the previous transparent behavior.
- **reject rules** — an invocation whose joined args match `match` (same glob semantics as allow/deny) is rejected by both the shim (early) and the broker (authoritative), surfacing `host_commands.<name>: rejected: <reason>` to the agent. Write `reason` as actionable guidance (what to do instead), not just "not allowed".

`local/<name>` kit references (e.g. `local/my-kit`) resolve to a kit directory relative to the project root, allowing local kit development without publishing to a remote registry.

### BindMount

Each `additional_bindings` entry mounts a path from the host into the sandbox.

```yaml
additional_bindings:
  - source: ${HOME}/.local/share/some-tool
  - source: ${HOME}/.config/some-tool
    mode: rw
  - source: ${HOME}/.netrc
    is_file: true
    optional: true
  # bind a gitignored file from the project root into the worktree
  - source: ${PROJECT_WORKDIR}/global.json
    target: ${WORKTREE}/global.json
    is_file: true
    optional: true
```

| Key | Type | Default | Role |
|---|---|---|---|
| `source` | string | (required) | Host path. Supports `${VAR}` expansion. |
| `target` | string | same as `source` | Mount path inside the sandbox. |
| `mode` | string | `""` (read-only) | `rw` for read-write; empty string is read-only. |
| `is_file` | bool | `false` | Set to `true` if `source` is a single file. |
| `optional` | bool | `false` | If `true`, skip silently when `source` does not exist on the host. |

#### Dynamic tokens: `${WORKTREE}` / `${PROJECT_WORKDIR}`

In addition to regular environment variables (`${HOME}`, etc.), `source` and `target` support two tokens that boid resolves at dispatch time:

- `${PROJECT_WORKDIR}` — the host-side project directory (e.g. `/home/you/src/your-project`).
- `${WORKTREE}` — the sandbox working directory for this task. For project-visible jobs (project cloned into the sandbox through the git gateway), this is the in-sandbox clone path (e.g. `/workspace/<project-name>`); for jobs where the project is not visible, it equals `${PROJECT_WORKDIR}`.

A binding whose resolved `target` equals its resolved `source` is skipped automatically (self-mount prevention).

> **Note:** `workspace.yaml` bindings require an explicit `mode` value (`ro` or `rw`). An empty `mode` string is not accepted.

### Instruction

The shape of the `default_instruction` value.

```yaml
default_instruction:
  agent: claude-code
  model: sonnet
  message: |
    ...
```

| Key | Type | Role |
|---|---|---|
| `agent` | string | Selects the harness and routing target for this instruction. `claude-code` → claude harness (built into boid core); `codex` → built-in codex adapter; `opencode` → built-in opencode adapter. Unrecognised or empty values fall through to the shell adapter. |
| `name` | string | Optional sub-identifier when several instructions go to the same agent. |
| `message` | string | The instruction text given to the agent. |
| `model` | string | Model selector the kit will pass through (e.g. `opus`, `sonnet`). |

> **Note:** `type:` and `interactive:` are not fields of `Instruction` and are silently ignored if present in YAML.

### CommandSpec (removed)

Phase 3-d (2026-06 release) retired the `commands:` map. Any `commands:` entries left in `project.yaml` (top level or under `task_behaviors.<name>`) are **silently ignored, with a single deprecation warning emitted at load time** (boid daemon log). Existing yaml keeps loading.

Migration:

| Old | New |
|---|---|
| `boid exec <project_id> <command-name>` invokes a named registered command | `boid exec -p <project_id> -- <argv...>` runs an arbitrary argv directly |
| Web UI **Commands** button starts a claude session | `/sessions/new` lets you pick the harness (claude / codex / opencode) and starts a session. The same path is exposed as `POST /api/projects/{id}/sessions`. |
| Task-detail **Commands** button runs behavior commands | Long-running task flows belong in behavior hooks. One-off runs that don't need a task can use `boid exec`. |

## capabilities

Top-level field for enabling optional sandbox capabilities.

### `capabilities.docker`

Declaring `capabilities.docker: {}` enables the **native Docker proxy** for the project's sandboxes.

```yaml
capabilities:
  docker: {}   # empty object is the opt-in marker
```

When enabled the boid daemon automatically:

1. Starts a per-sandbox proxy socket (`/run/boid/docker-proxy.sock`)
2. Bind-mounts the socket into the sandbox
3. Sets the following environment variables in the sandbox

| Variable | Value |
|---|---|
| `DOCKER_HOST` | `unix:///run/boid/docker-proxy.sock` |
| `CONTAINER_HOST` | `unix:///run/boid/docker-proxy.sock` |
| `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` | `/run/boid/docker-proxy.sock` |
| `TESTCONTAINERS_RYUK_DISABLED` | `true` |

The Docker CLI, Docker SDKs, and TestContainers all respect `DOCKER_HOST`, so they work through the proxy without additional configuration. `TESTCONTAINERS_RYUK_DISABLED=true` disables the TestContainers Ryuk reaper (Ryuk requires a docker.sock bind-mount that the proxy's sandbox isolation prohibits; boid cleans up containers on job exit instead).

For the proxy's security model, body-inspection rules, and container GC details, see [Sandbox internals / Docker proxy](../architecture/sandbox-internals.md#docker-proxy-capabilitiesdocker).

#### docker CLI and host_commands

Docker commands inside the sandbox run through the proxy socket (`DOCKER_HOST`). **When `capabilities.docker` is enabled, registering `docker` in `host_commands` without subcommand restrictions is an error.** An unrestricted `docker` entry in `host_commands` means host-side direct execution (bypassing the proxy), which boid rejects at job launch:

```
host_commands.docker: unrestricted docker access bypasses the docker proxy
(capabilities.docker is enabled); remove docker from host_commands or restrict
to specific subcommands (e.g. allow: [build])
```

If image builds must run on the host, restrict to the `build` subcommand:

```yaml
host_commands:
  docker:
    allow: [build]   # host-side docker build only
```

Note that host-side execution still carries the risks of `--network host`, `--secret`, etc. For ordinary `docker run` and TestContainers usage the proxy is sufficient — no `host_commands` entry is needed.

#### Rootless Docker recommendation

The proxy is the primary defence layer. To limit the blast radius of a proxy bypass, we strongly recommend running the host Docker daemon in **rootless mode**. Rootless Docker confines containers to a user namespace, so host-root escalation is structurally impossible.

```sh
# Set up rootless Docker (one time)
curl -fsSL https://get.docker.com/rootless | sh
# or via distro package: apt install docker-ce-rootless-extras
```

boid resolves the upstream socket at startup in this order: `DOCKER_HOST` environment variable → rootless path (`$XDG_RUNTIME_DIR/docker.sock`) → rootful `/var/run/docker.sock`.

For migration from the docker kit (cetusguard-based) to the native proxy, see the [Docker proxy migration guide](../guide/docker-proxy-migration.md).

## Project-local overrides (`.boid/project.local.yaml`) — Deprecated

> **Deprecated**: `project.local.yaml` has been removed. Its settings move to `workspace.yaml`.
> Run `boid project migrate <dir>` to convert automatically. See the [Migration guide](../guide/migration.md).

The `host_commands` / `additional_bindings` / `env` / `secret_namespace` fields that `project.local.yaml` used to provide are now set in `workspace.yaml` (machine-local, `gitignore`d).

## Example: a real project

An excerpt from `.boid/project.yaml` in the `boid` repository itself, showing the two behaviors (`supervisor`, `executor`).

```yaml
id: boid
name: boid

# worktree is a legacy field (no-op since branch-policy-simplification Phase
# 1): every task, root or child, checks out base_branch directly on its
# in-sandbox clone regardless of this value — no host-side git worktree is
# created.
worktree: true

kits:
  - github.com/novshi-tech/boid-kits/claude-code
  - github.com/novshi-tech/boid-kits/go-dev
  - github.com/novshi-tech/boid-kits/github-cli

host_commands:
  playwright-cli:
    allow: ['*']
  run-e2e:
    path: e2e/run.sh

task_behaviors:
  executor:
    name: executor
    default_instruction: { ... }
  supervisor:
    name: Supervisor
    default_instruction: { ... }
```

For a fuller example — and three different workflow shapes built on top of this schema — see [Workflows](../../workflows.md).
