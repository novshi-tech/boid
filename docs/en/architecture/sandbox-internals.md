# Sandbox internals

How `boid`'s sandbox is built, and what actually happens when one hook runs. This is the file-and-syscall-level zoom of the sandbox section in the [Architecture overview](overview.md).

The intended readers are contributors who touch `internal/sandbox/`, anyone debugging a sandbox-shaped bug, or anyone who wants to know exactly *why* their home directory is invisible from inside.

## What the sandbox enforces

The sandbox draws four boundaries simultaneously:

1. **Filesystem.** Writable areas are confined to the in-sandbox project clone (or, for jobs where no project is visible, the project root).
2. **Network.** Only domains in the built-in allowlist or `config.yaml`'s `sandbox.allowed_domains` can be reached.
3. **User ID.** The host's `root` is unreachable (rootless).
4. **Commands.** Only host commands declared in the kit's `host_commands` cross the boundary.

All of this is built from stock Linux primitives (mount namespace, user namespace, chroot, pasta, nftables). No extra runtime like Docker is required.

## The launch chain

When the daemon starts a hook, the dispatcher writes a JSON spec file to disk and forks `boid runner-outer`. The full chain is five levels deep:

```
+-------------------------------------------------------------+
| runner-outer  (runs on the host)                            |
|   reads the JSON spec                                       |
|   forks pasta as a child process  --------+                 |
+-------------------------------------------|------------------+
                                            v
+-------------------------------------------------------------+
| pasta (network namespace + user namespace)                  |
|   -- forks boid runner-inner  ------------+                 |
+-------------------------------------------|------------------+
                                            v
+-------------------------------------------------------------+
| runner-inner  (inside pasta's user+net ns, inner uid 0)     |
|   applies nftables egress rules                             |
|   clone(CLONE_NEWUSER|CLONE_NEWNS) → runner-inner-child -+ |
+-----------------------------------------------------------|--+
                                                           v
+-------------------------------------------------------------+
| runner-inner-child  (new user+mount ns, uid 0)              |
|   bind-mount sandbox fs into $ROOT                          |
|   pivot_root into $ROOT                                     |
|   write context files to $HOME/.boid/context/              |
|   adapter.Run() → exec the agent                            |
+-------------------------------------------------------------+
```

The implementation lives in [`internal/sandbox/runner/`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/runner/) (replaced the former bash trio in Phase 3-a).

### 1. `runner-outer`

Reads the JSON spec and launches pasta with:

```
pasta --config-net -4 \
    -a 10.0.2.0 -n 24 -g 10.0.2.2 \
    --dns-forward 10.0.2.3 \
    -t none -u none \
    -- boid runner-inner --spec <spec.json> --state <state.json>
```

`pasta` is a user-mode network-namespace wrapper. It gives the sandbox its own network stack — the host's NICs are not visible. Outbound traffic is relayed through pasta's gateway (`10.0.2.2`) and DNS forwarder (`10.0.2.3`). After pasta returns, `runner-outer` performs host-side cleanup (see [Cleanup](#cleanup)).

### 2. `runner-inner`

Runs inside pasta's user+network namespace as **inner uid 0** (uid_map: host uid 1000 → container uid 0; uid 0 is required to hold `CAP_SYS_ADMIN` for the subsequent mount operations).

Main steps:

- **nftables rules** — while holding uid 0 and `CAP_NET_ADMIN`, programmes the egress drop rules (proxy port only).
- **`clone(CLONE_NEWUSER|CLONE_NEWNS)`** — forks `runner-inner-child` into a fresh user+mount namespace with the same uid_map (`ContainerID=0, HostID=<euid>, Size=1`).

### 3. `runner-inner-child`

Runs in the new user+mount namespace (uid 0). Builds the sandbox filesystem and launches the agent.

Main steps:

- **bind mounts** — the kit's `additional_bindings`, the in-sandbox clone's runtime directory (when the project is visible), and system directories (`/usr`, `/lib`, etc.) are bind-mounted (or rbind-mounted) into `$ROOT`. This determines the file set visible inside the sandbox.
- **`pivot_root`** — switches the root to `$ROOT`; the old root is pivoted to `/.old_root` then unmounted and removed.
- **context files** — after `pivot_root`, writes `$HOME/.boid/context/{task,instructions,environment,payload}.{yaml,json}` from the spec.
- **symlinks** — the `boid` shim is symlinked at `/opt/boid/bin/<command>` etc.
- **`adapter.Run()`** — invokes the HarnessAdapter (claude / codex / opencode / shell) to exec the agent, relay the stop signal (SIGUSR1 → SIGTERM to the agent), normalise the exit code, and post the broker job-done via `brokerclient`. (`shell` is the fall-through adapter used by `boid exec` and non-agent hook scripts; the `boid agent shell` session variant was retired after the git gateway cutover.)

From inside the sandbox:

- The home directory, SSH keys, and other projects do not exist (paths don't resolve unless bind-mounted into `$ROOT`).
- The process runs as uid 0 inside the user namespace but cannot escape it — there is no escalation path to the host root.

Task context is available through the context files at `$HOME/.boid/context/`. The handler-side protocol is documented in [Hook script protocol](../reference/hook-contract.md).

## Network control

Network containment has two layers.

### ① pasta (the network namespace)

pasta is a user-privilege network-namespace wrapper. The sandbox sees only pasta's virtual network; the host's physical NICs are invisible. Outbound traffic is relayed back to the host by pasta itself.

### ② nftables drop rules

`runner-inner` programmes nftables while holding uid 0 to drop all outbound traffic except to the proxy port. The end result:

- HTTP/HTTPS goes only through `http_proxy` / `https_proxy` pointing at `10.0.2.2:<port>`.
- The proxy forwards only those domains in the allowlist (see below).
- Other TCP/UDP is blocked.

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
inside sandbox: boid <subcommand>      (shim binary)
                  |
                  | UNIX socket (on the host)
                  v
host: boid daemon's broker (internal/sandbox.Broker)
                  |
                  | evaluates the host-command policy
                  v
host: actually exec the allowed command
```

The shim is bind-mounted into the sandbox at startup — it is a small binary built in `internal/sandbox/boid_shim.go`. `boid task update`, `boid job done`, and any commands the kit declared in `host_commands` (`gh`, `git push`, ...) all flow over this path.

The broker lives in `internal/sandbox/broker.go` and is responsible for:

- Accepting requests from the shim over the UNIX socket.
- Looking up the **token** attached to the request to identify which job is calling.
- Checking the call (command, subcommand, arguments) against the policy in `policy.go` via `CheckPolicy`.
- If allowed, exec'ing on the host and streaming stdout / stderr / exit code back to the shim.

The token is issued at sandbox start and passed in via environment variables such as `BOID_BROKER_TOKEN`. Outside the sandbox the token is unknown, so even if the broker socket path leaks, another job's commands cannot be authorised.

Host commands run on the host in a neutral directory (`os.TempDir()`), never any project checkout, and stdin is never forwarded. Commands that need repo context (e.g. `gh`) get it via a kit `env:` entry of `${boid:repo_slug}` (see "Host command execution contract" in the [`project.yaml` reference](../reference/project-yaml.md)).

## Cleanup

Cleanup runs in **`runner-outer`** (Go code) after pasta returns:

```go
// runner-outer (excerpt)
cleanupRoot(spec.RootDir)          // rm -rf guarded by /tmp/boid-root-* prefix
for _, p := range spec.CleanupPaths { os.RemoveAll(p) }
os.Remove(specPath)                // spec contains secrets; remove unconditionally
if exitCode == 0 { os.Remove(statePath) }  // state file retained on failure
```

Three points:

1. **The kernel reclaims mounts.** `runner-inner` and `runner-inner-child` ran inside namespaces owned by pasta's process tree. When pasta exits, those namespaces are destroyed and all bind mounts underneath are automatically reclaimed by the kernel — no explicit `umount` is needed.
2. **`$ROOT` is removed from outside the sandbox namespace.** By the time `runner-outer` runs its cleanup, the sandbox mount namespace is already gone, so `rm -rf` only sees empty scaffolding on the host and cannot traverse into any bind-mounted host content.
3. **`/tmp/boid-root-*` prefix guard.** `cleanupRoot` skips and logs a warning if `spec.RootDir` does not start with `/tmp/boid-root-`, preventing accidental host damage from misconfiguration.

On failure, a `runner-state.json` file (`/tmp/boid-<runtime_id>-runner-state.json`) is kept for post-mortem diagnosis. It contains the phase-level progress log, the spec (with secrets redacted), and the exit code. The file is removed by the daemon's 30-day GC cycle. The spec file is always removed regardless of exit code (it carries the broker token and other secrets).

A previous regression where bind mounts traversed during `rm -rf` deleted host files motivated the current design — both the own-namespace and cross-namespace paths now fail closed.

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

> **Note:** `task.reopen` uses a `.` separator for historical reasons; all other ops use `_`.

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
