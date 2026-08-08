# Sandbox internals

How `boid`'s sandbox is built, and what actually happens when one hook runs. This is the container-launch-parameters-and-filesystem-layout-level zoom of the sandbox section in the [Architecture overview](overview.md).

The intended readers are contributors who touch `internal/sandbox/`, anyone debugging a sandbox-shaped bug, or anyone who wants to know exactly *why* their **host** home directory is invisible from inside (the sandbox's own `$HOME` is a different, workspace-scoped thing — see the "From inside the sandbox" section below).

## What the sandbox enforces

The sandbox draws four boundaries simultaneously:

1. **Filesystem.** Writable areas are confined to the in-sandbox project clone (or, for jobs where no project is visible, the project root).
2. **Network.** Only domains in the built-in allowlist or `config.yaml`'s `sandbox.allowed_domains` can be reached.
3. **User ID.** The host's `root` is unreachable (rootless).
4. **Commands.** Only host commands declared in the kit's `host_commands` cross the boundary.

All of this is delegated to a Docker/Podman container runtime by the boid daemon (as of PR-4 = the volume-only cutover, the container backend is the sole sandbox backend — `sandbox.backend` config was removed). Mount/network/user namespace isolation and the root filesystem switch are handled entirely by the container runtime; boid no longer issues any namespace-related syscalls directly.

## The launch chain

When the daemon starts a hook, `internal/dispatcher`'s `containerBackend.Launch` ([`internal/dispatcher/container_backend.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/container_backend.go)) translates a `sandbox.Spec` into a Docker Engine API `container create` call, asking the host-side docker/podman daemon to start a sibling container of the boid daemon itself (docker-out-of-docker).

```
+-------------------------------------------------------------+
| boid daemon (bare-metal, or the compose daemon service)     |
|   containerBackend.Launch translates sandbox.Spec into a    |
|   docker `container create` + `start` call                  |
+----------------------------------|---------------------------+
                                    v  Docker Engine API
+-------------------------------------------------------------+
| job container (image built by build/container/Dockerfile,   |
| boid-runner; `HostConfig.Init: true` makes docker-init/tini  |
| PID 1)                                                        |
|   ENTRYPOINT = `/usr/local/bin/boid runner-container`        |
|     --spec /run/boid/spec.json --state /run/boid/state.json |
|   writes spec.Files → materialises spec.Symlinks (host       |
|   command shims) → in-sandbox clone (if declared) →          |
|   adapter.Run() execs the agent                              |
+-------------------------------------------------------------+
```

The five-level process chain the former userns backend used (`runner-outer → pasta → runner-inner → runner-inner-child`; `cmd/runner.go`'s `runner-outer`/`runner-inner` subcommands, `internal/sandbox/runner/runner_linux.go`'s `clone(CLONE_NEWUSER|CLONE_NEWNS)` + `pivot_root` path) was removed entirely in PR-4 (`docs/plans/volume-only-daemon.md`, the 2026-07 cutover). The implementation is now split between the host-side [`internal/dispatcher/container_backend.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/container_backend.go) (launch / reap, exposed via the `backend.SandboxBackend` interface), [`internal/dispatcher/container_session.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/container_session.go) (attach / resize / signal, exposed via `backend.SandboxSession`), and [`internal/sandbox/runner/runner_container_linux.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/runner/runner_container_linux.go)'s `RunContainer`, which runs inside the container as the `boid runner-container` entry point.

### Container launch parameters (`containerBackend.Launch`)

- **Image** — the boid-runner image built by `build/container/Dockerfile`. The `boid` binary is baked into `/usr/local/bin/boid` at build time (`COPY --from=builder`); `ENTRYPOINT` is fixed at `["/usr/local/bin/boid", "runner-container"]`; `Cmd` carries only the `--spec`/`--state` pair.
- **spec / state files** — only the `runner-spec.json` (read-only) / `runner-state.json` (read-write) the dispatcher wrote host-side are bind-mounted, at `/run/boid/spec.json` / `/run/boid/state.json` respectively. These two files are the only host filesystem visible from the job container — there is no project-checkout or home-directory bind mount (a direct consequence of the volume-only pivot; see `docs/plans/volume-only-daemon.md`).
- **Volumes** — [`internal/sandbox/realization/`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/realization/) resolves volume/tmpfs entries into Docker named-volume/mount entries (`containerMounts`). The workspace-scoped `$HOME` is also a named volume (see below).
- **Network** — connects to a disposable, per-workspace `internal: true` docker network the daemon creates on demand (see [Network control](#network-control) below).
- **PID 1** — `HostConfig.Init: true` (docker-init/tini). SIGUSR1 relay and zombie reaping are tini's job; boid no longer implements its own signal-forwarding loop.
- **uid/gid** — runs as the fixed non-root uid/gid (the `boid` user) created at image-build time.

### The `boid runner-container` entrypoint (`RunContainer`)

The job container's root filesystem already *is* the image's own filesystem (the sandbox root) from the moment the process starts, so there is nothing equivalent to `pivot_root` to do. `RunContainer`'s steps:

1. Write every `spec.Files` entry at its absolute path.
2. Materialise `spec.Symlinks` (per-project host-command shims, `/run/boid/bin/<name> -> boid`) — the image bakes in only the `boid` symlink itself (the set of allowed commands is unknown at image-build time), so every container start re-derives them fresh from the spec.
3. Run the in-sandbox clone when `spec.Clone.Enabled` (over the git gateway; no broker dispatch to the daemon).
4. Apply `spec.Env["PATH"]`.
5. Invoke `adapter.Run()` (via the HarnessAdapter) to exec the agent (claude / codex / opencode) or the shell hook, relaying the stop signal (SIGUSR1 → SIGTERM to the agent) and normalising the exit code.
6. Post `boid job done` to the broker afterward (see [Host commands and the broker](#host-commands-and-the-broker) below).

From inside the sandbox:

- The *host's* home directory, SSH keys, and other projects do not exist (there is no host filesystem bind mount besides the spec/state pair). The sandbox's own `$HOME` is a different thing entirely — see below.
- There is no path out of the container — to the host or to another job's container — because the container runtime's own namespace isolation provides it.

`$HOME` inside the sandbox is not host-shared and not a fresh tmpfs either: it is a **workspace-scoped named volume, mounted read-write, that persists across every job dispatched against the same workspace** (docs/plans/home-workspace-volume.md Phase 4). A file a hook writes under `$HOME` is visible to a later, unrelated job in the same workspace. `$HOME/.boid` persists the same way — before Phase 6 PR8 it was a fresh, job-scoped tmpfs on every dispatch to keep `$HOME/.boid/output/payload_patch.json` from leaking between jobs sharing a workspace; now that the sole payload-patch path is the broker RPC (`boid task update --payload-patch`), that file-based output was retired and the isolating tmpfs with it (see [Hook script protocol / Outputs](../reference/hook-contract.md#outputs)).

Task context is available by calling `boid task current` / `instructions` / `env` / `payload` — broker RPCs reachable over the shim, pulled on demand rather than materialized at dispatch time. The handler-side protocol is documented in [Hook script protocol](../reference/hook-contract.md).

## Network control

Network containment has two layers.

### ① a per-workspace docker internal network

Each workspace gets a disposable network the daemon creates with `docker network create --internal --label boid.workspace=<slug>` (`ensureWorkspaceNetwork`, [`internal/dispatcher/container_backend.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/container_backend.go)), and every job container for that workspace is connected to it. `internal: true` means containers on this network have no default route to the outside world — a job container can only reach the boid daemon (itself) on the same network directly.

### ② the daemon's built-in egress proxy

The daemon container itself runs [`internal/sandbox/proxy_manager.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/proxy_manager.go)'s ProxyManager in-process, reachable on the compose network under the `boid-egress` DNS alias (`build/container/compose.yml`). The job container gets the daemon's address via `http_proxy` / `https_proxy` / `HTTP_PROXY` / `HTTPS_PROXY` env vars (`applyProxyEnv`, [`internal/dispatcher/sandbox_builder.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/sandbox_builder.go)); requests to domains not on the allowlist are rejected by the proxy. Direct TCP/UDP is already blocked by the internal network having no default route. The daemon container is separately attached to the default bridge network too, which is where it gets its own outbound internet access — both for the proxy's own upstream fetches and for the `docker pull`s it issues when creating job containers (see the topology notes in `build/container/compose.yml`).

The proxy itself lives in [`internal/sandbox/proxy.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/proxy.go) and runs as a goroutine inside the daemon.

#### Proxy allowlist

The allowed domains come from two sources merged at daemon startup:

1. **Built-in list** — Anthropic/OpenAI API endpoints, language package registries, Docker Hub, and similar; hardcoded in `cmd/start.go`'s `defaultAllowedDomains()`.
2. **User additions** — entries in `sandbox.allowed_domains` in `~/.config/boid/config.yaml`, appended to the built-in list.

```yaml
# ~/.config/boid/config.yaml
sandbox:
  allowed_domains:
    - ".github.com"      # leading dot = suffix match
    - "api.example.com"  # no dot = exact match
```

Changes take effect after `boid stop && boid start`.

## Docker proxy (`capabilities.docker`)

When `capabilities.docker: {}` is declared in `project.yaml`, the boid daemon starts a **Docker proxy** for each sandbox and interposes it between sandbox processes and the upstream Docker daemon. The implementation lives in [`internal/sandbox/dockerproxy/`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/dockerproxy/).

```
sandbox process (docker CLI / SDK / TestContainers)
        |
        | DOCKER_HOST=unix:///run/boid/docker-proxy.sock
        v
[Docker Native Proxy]  (internal Unix socket)
        |
        | policy evaluation (policy.go)
        v
upstream Docker daemon (/run/user/<uid>/docker.sock etc.)
```

### Routing: fail-closed

The routing rules are **fail-closed** ([`server.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/dockerproxy/server.go)):

| Request | Action |
|---|---|
| `GET` / `HEAD` (all endpoints) | transparent forward (read-only) |
| explicitly-allowed mutating endpoints | transparent forward |
| mutating endpoints requiring body inspection | inspect then ALLOW / DENY |
| `POST /build`, `POST /session` (image build) | fixed deny |
| any other unknown mutating endpoint | default deny (fail-closed) |

Image build is denied because BuildKit hijacks the `/session` connection to run gRPC, making body inspection impossible.

### Body inspection: denied HostConfig settings

`POST /containers/create` bodies are inspected in detail ([`policy.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/dockerproxy/policy.go)). The following settings return `403 Forbidden`:

| Field | Deny condition | Error message |
|---|---|---|
| `HostConfig.Binds` | one or more entries | `HostConfig.Binds: bind mounts are not permitted` |
| `HostConfig.Mounts` | any entry with `Type=bind` | `HostConfig.Mounts: type=bind mount is not permitted` |
| `HostConfig.Mounts` | `Type=volume` + `VolumeOptions.DriverConfig.Options.device` | `HostConfig.Mounts: volume with device option (system 3 bind) is not permitted` |
| `HostConfig.Mounts` | `Type=volume` + `Options.o` contains `bind` | `HostConfig.Mounts: volume with o=bind option (system 3 bind) is not permitted` |
| `HostConfig.Privileged` | `true` | `HostConfig.Privileged: privileged containers are not permitted` |
| `HostConfig.NetworkMode` | `host` / `container:<id>` / `ns:<path>` | `HostConfig.NetworkMode: <value> is not permitted` |
| `HostConfig.PidMode` | `host` / `container:<id>` / `ns:<path>` | `HostConfig.PidMode: <value> is not permitted` |
| `HostConfig.IpcMode` | `host` / `container:<id>` / `ns:<path>` | `HostConfig.IpcMode: <value> is not permitted` |
| `HostConfig.UsernsMode` | `host` | `HostConfig.UsernsMode: host is not permitted` |
| `HostConfig.CgroupnsMode` | `host` | `HostConfig.CgroupnsMode: host is not permitted` |
| `HostConfig.SecurityOpt` | one or more entries (any value) | `HostConfig.SecurityOpt: security options are not permitted` |
| `HostConfig.CapAdd` | one or more entries (any capability name) | `HostConfig.CapAdd: adding capabilities is not permitted` |
| `HostConfig.Devices` | one or more entries | `HostConfig.Devices: device access is not permitted` |
| `HostConfig.DeviceCgroupRules` | one or more entries | `HostConfig.DeviceCgroupRules: device cgroup rules are not permitted` |
| `HostConfig.Runtime` | anything other than `runc` | `HostConfig.Runtime: only runc runtime is permitted, got <value>` |
| `HostConfig.Sysctls` | one or more entries | `HostConfig.Sysctls: sysctl settings are not permitted` |
| `HostConfig.CgroupParent` | non-empty | `HostConfig.CgroupParent: custom cgroup parent is not permitted` |

`POST /containers/{id}/exec` denies `Privileged=true`.
`POST /containers/{id}/start` denies requests that carry a non-empty `HostConfig` (legacy API form).
`POST /networks/create` denies `Driver=host`.
`POST /volumes/create` denies `DriverOpts.device` and `DriverOpts.o` containing `bind`.

The proxy **forwards the raw received bytes verbatim** — it never decodes and re-encodes the body. This prevents parser-differential attacks where a crafted body would be parsed differently by the proxy and the upstream daemon.

### Container GC (Ryuk replacement)

TestContainers' Ryuk reaper requires a docker.sock bind-mount, which the proxy prohibits. `TESTCONTAINERS_RYUK_DISABLED=true` is set automatically to disable Ryuk; boid takes over the cleanup role.

- **ID recording**: For creation endpoints (`POST /containers/create`, `/networks/create`, `/volumes/create`) the proxy reads the ID from the upstream response and appends it to `<runtimes-dir>/<runtime_id>/docker-resources.jsonl` with fsync — **before returning the response to the client** — so that "every ID the client knows is already in the ledger" ([`ledger.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/dockerproxy/ledger.go)).
- **Synchronous cleanup**: On job exit (success or failure) `Reap()` reads the ledger and issues `stop` + `rm` for containers, then networks, then volumes ([`reap.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/dockerproxy/reap.go)).
- **GC safety net**: The daemon's 24-hour GC loop cleans up ledger resources before removing their runtime directory, recovering orphaned resources from daemon crashes or other missed cleanups.

### Job isolation (ID scope check)

A rootless Docker upstream daemon is shared across all jobs for the same UID. The proxy restricts access using the ledger: **only resource IDs created by the current job's proxy are allowed**.

- Endpoints with an `{id}` segment (`/containers/{id}/`, `/networks/{id}/`, `/volumes/{name}/`, `/exec/{id}/`) are only forwarded when the ID is in the current job's ledger.
- Operations on an ID not in the ledger return **404**, hiding the existence of other jobs' resources ([`server.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/dockerproxy/server.go)).

### Environment variables injected

When `capabilities.docker` is enabled, the following variables are set in the sandbox ([`sandbox_builder.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/sandbox_builder.go)):

| Variable | Value |
|---|---|
| `DOCKER_HOST` | `unix:///run/boid/docker-proxy.sock` |
| `CONTAINER_HOST` | `unix:///run/boid/docker-proxy.sock` |
| `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE` | `/run/boid/docker-proxy.sock` |
| `TESTCONTAINERS_RYUK_DISABLED` | `true` |

### Restriction: no unrestricted docker in host_commands

Registering `docker` in `host_commands` without subcommand restrictions is rejected at job launch when `capabilities.docker` is active ([`runner.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/runner.go) `validateDockerHostCommands`):

```
host_commands.docker: unrestricted docker access bypasses the docker proxy
(capabilities.docker is enabled); remove docker from host_commands or restrict
to specific subcommands (e.g. allow: [build])
```

An unrestricted entry would let sandbox processes run the real `docker` binary directly on the host, bypassing the proxy entirely. Entries restricted with `AllowedSubcommands` or `AllowedPatterns` (e.g. `allow: [build]`) are accepted.

## Host commands and the broker

To call a host-side command from inside the sandbox, two pieces work together: the `boid` shim and the broker.

```
inside the job container: boid <subcommand>  (the boid binary baked into the image)
                            |
                            | TCP + mTLS (over the workspace's docker network)
                            v
daemon container: boid daemon's broker (internal/sandbox.Broker)
                            |
                            | evaluates the host-command policy
                            v
daemon container: actually exec the allowed command
```

The shim *is* the `boid` binary itself (a multi-call binary that switches into shim behavior by subcommand, `internal/sandbox/boid_shim.go`), already baked into the image at `/usr/local/bin/boid` at build time. Per-project host-command name symlinks (`/run/boid/bin/<name> -> boid`) are re-materialised from `spec.Symlinks` on every container start (see "The `boid runner-container` entrypoint" above). Because the job container and the daemon container are separate processes (separate containers), the broker connection is TCP + mTLS rather than a UNIX socket — the daemon issues a short-lived client certificate per job (delivered via `BOID_BROKER_TLS_*` env vars), and `internal/sandbox/brokerclient` uses it to connect (docs/plans/phase6-cutover-followups.md §⓪, the broker TCP wire). `boid task update`, `boid job done`, and any commands the kit declared in `host_commands` (`gh`, `git push`, ...) all flow over this path.

The broker lives in `internal/sandbox/broker.go` and is responsible for:

- Accepting requests from the shim (over TCP + mTLS from the job container, or a UNIX socket on the bare-metal path).
- Looking up the **token** attached to the request to identify which job is calling.
- Checking the call (command, subcommand, arguments) against the policy in `policy.go` via `CheckPolicy`.
- If allowed, exec'ing on the daemon side and streaming stdout / stderr / exit code back to the shim.

The token is issued at sandbox start and passed in via environment variables such as `BOID_BROKER_TOKEN`. Outside the sandbox the token is unknown, so even if the broker's address or mTLS cert directory path leaks, another job's commands cannot be authorised.

Host commands run daemon-side in a neutral directory (`os.TempDir()`), never any project checkout, and stdin is never forwarded. Commands that need repo context (e.g. `gh`) get it via a kit `env:` entry of `${boid:repo_slug}` (see "Host command execution contract" in the [`project.yaml` reference](../reference/project-yaml.md)).

## Cleanup

Cleanup reduces to **stopping and removing the job container**. Because the container runtime itself owns creating and tearing down the mount/network/user namespaces, the former userns backend's concerns — "let the kernel reclaim the mount namespace", "remove `$ROOT` from the own-namespace vs. cross-namespace side" — no longer apply.

When a job container exits, `containerSession.waitLoop` ([`internal/dispatcher/container_session.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/container_session.go)):

1. Calls `ContainerRemove` (`RemoveVolumes: true`) to remove the container itself and any anonymous volume created for it (retrying with `Force: true` on failure).
2. Removes the `spec.json` / `state.json` (and any per-job TLS cert scratch directory) it wrote host-side — `spec.json` is always removed regardless of exit code, since it carries the broker token and other secrets.
3. Leaves the workspace's docker network and named volumes alone (later jobs on the same workspace reuse them).

On daemon startup, `containerBackend.ReapOrphans` sweeps up orphaned containers/networks/volumes left behind by a daemon restart or crash, using `boid.*` labels (called from `internal/server/wire.go`'s `reapOrphansBeforeReopen`).

On failure (`exitCode != 0`), the `state.json` (`runner-state.json`) is kept rather than removed, for post-mortem diagnosis. It contains the phase-level progress log, the spec (with secrets redacted), and the exit code.

## Allowed boid builtins from inside the sandbox

Handlers running inside the sandbox (hook, exec) can call three built-in commands: `boid`, `git`, and `fetch`.
All are injected automatically — no declaration in `project.yaml` / `kit.yaml` is needed.

### boid builtin

All roles (hook) share the same allowed op set — there is no role branching.

| Op (sandbox protocol) | Corresponding CLI | Purpose |
|---|---|---|
| `job_done` | `boid job done <id>` | Notify the daemon that this job completed |
| `job_list` | `boid job list --task <id>` | List jobs belonging to a task |
| `job_show` | `boid job show <id>` | Show job detail |
| `job_log` | `boid job log <id>` | Retrieve job execution log |
| `action_send` | `boid action send` | Dispatch a manual action |
| `agent_stop` | `boid agent stop <job-id>` | Send SIGUSR1 to a running agent job |
| `task_create` | `boid task create` | Create a child task |
| `task_get` | `boid task show <id> --field <path>` | Read a single task field (dotted JSON path) |
| `task_update` | `boid task update <id>` | Update task fields |
| `task_import` | `boid task import` | Bulk-import tasks |
| `task.reopen` | `boid task reopen <id>` | Transition a done task back to executing |
| `task_list` | `boid task list` | List tasks in the workspace |
| `task_notify` | `boid task notify <id>` | Send a notification or Q&A (`--ask`) |
| `task_answer` | `boid task answer` | Transition awaiting → executing |
| `task_delete` | `boid task delete <id>` | Delete a task |
| `task_current` | `boid task current` | Read this task's id/title/description/status/behavior/readonly |
| `task_instructions` | `boid task instructions` | Read this job's own routed instruction |
| `task_env` | `boid task env` | Read `allowed_domains` + `host_commands` (the properties this sandbox cannot observe on its own) |
| `task_payload` | `boid task payload` | Read the trait-filtered current payload |
| `task_attachments_list` | `boid task attachments list` | List this task's attachment filenames |
| `task_attachments_get` | `boid task attachments get <name>` | Fetch one attachment's bytes |
| `project_behaviors` | `boid project behaviors <project-ref>` | Fetch a project's task_behaviors as JSON (`ref` only resolves to a project within the caller's own workspace) |
| `project_list` | `boid project list` | List projects (id/name/upstream_url) within the caller's own workspace, as JSON. Takes no arguments; scope is fixed to the token's `AllowedProjectIDs` |

> **Note:** `task.reopen` uses a `.` separator for historical reasons; all other ops use `_`. `task_current`/`task_attachments_list`/`task_attachments_get` are TaskID-scoped; `task_instructions`/`task_env`/`task_payload` are JobID-scoped (see [Hook script protocol](../reference/hook-contract.md)).

### fetch builtin

`boid fetch <url>` performs an HTTP GET from inside the sandbox through the proxy allowlist. Useful for retrieving web resources without requiring `host_commands` for `curl`/`wget`.

| Op | Corresponding CLI | Purpose |
|---|---|---|
| `fetch` | `boid fetch <url>` | HTTP GET through the outbound proxy |

### Design notes

- **No role branching** — `boid` and `fetch` policies use `_ Role`; every role gets the same op set.
  Add a role `switch` inside `policyFor` only when a new builtin genuinely needs role-specific restrictions.
- **Source of truth** — `internal/orchestrator/policy.go`, functions `boidPolicy` / `fetchPolicy`.
- **Sandbox-side enum** — `internal/sandbox/protocol.go`.
- **Cross-workspace access** is denied by the broker (`internal/sandbox/broker.go` `handleBoidBuiltin`)
  via `entry.Context.AllowsProject(...)` and similar guards — the op set above does not bypass these checks.

## Related documents

- [Architecture overview](overview.md) — where the sandbox layer sits.
- [Concepts / Sandbox](../guide/concepts.md#sandbox) — the user-visible meaning.
- [Hook script protocol](../reference/hook-contract.md) — the I/O contract for handlers running inside.
- [`project.yaml` reference](../reference/project-yaml.md) — declaring `host_commands` / `additional_bindings` / `capabilities`.
- [Docker proxy migration guide](../guide/docker-proxy-migration.md) — migrating from the docker kit (cetusguard) to the native proxy.
