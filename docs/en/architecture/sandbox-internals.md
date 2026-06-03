# Sandbox internals

How `boid`'s sandbox is built, and what actually happens when one hook runs. This is the file-and-syscall-level zoom of the sandbox section in the [Architecture overview](overview.md).

The intended readers are contributors who touch `internal/sandbox/`, anyone debugging a sandbox-shaped bug, or anyone who wants to know exactly *why* their home directory is invisible from inside.

## What the sandbox enforces

The sandbox draws four boundaries simultaneously:

1. **Filesystem.** Writable areas are confined to the worktree (or the project root).
2. **Network.** Only domains the kit declares can be reached.
3. **User ID.** The host's `root` is unreachable (rootless).
4. **Commands.** Only host commands declared in the kit's `host_commands` cross the boundary.

All of this is built from stock Linux primitives (mount namespace, user namespace, chroot, pasta, nftables). No extra runtime like Docker is required.

## The launch chain

When the daemon starts a hook, `internal/sandbox.Prepare` writes three shell scripts to `/tmp/boid-<job-id>-{outer,setup,inner}.sh`:

```
+-------------------------------------------------------------+
| outer.sh  (runs on the host)                                |
|   wraps the rest with pasta (network namespace), then       |
|     unshare --mount  ------+                                |
+----------------------------|--------------------------------+
                             v
+-------------------------------------------------------------+
| setup.sh  (inside the new mount namespace,                  |
|            host fs is still fully visible here)             |
|   bind-mount worktree, kit files, etc. into $ROOT           |
|   apply nftables rules to drop outbound traffic             |
|     (allowing only the proxy)                               |
|   exec unshare --user --map-user --root=$ROOT --            |
|     bash /tmp/inner.sh   ----+                              |
+------------------------------|------------------------------+
                               v
+-------------------------------------------------------------+
| inner.sh  (rootless, $ROOT is the only visible filesystem)  |
|   set environment variables, then run the handler script    |
+-------------------------------------------------------------+
```

The implementation lives in [`internal/sandbox/script.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/script.go) and [`internal/sandbox/render.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/render.go).

### 1. `outer.sh`

```bash
pasta --config-net \
    -a 10.0.2.0 -n 24 -g 10.0.2.2 \
    --dns-forward 10.0.2.3 \
    -t none -u none \
    -- unshare --mount -- bash /tmp/boid-<id>-setup.sh
```

`pasta` is a user-mode network-namespace wrapper. It gives the sandbox its own network stack — the host's NICs are not visible. Outbound traffic is relayed through pasta's gateway (`10.0.2.2`) and DNS forwarder (`10.0.2.3`).

Inside that, `unshare --mount` opens a new mount namespace. From this point on, bind mounts performed by the sandbox cannot leak into other host processes.

### 2. `setup.sh`

Right after `unshare --mount`, the host filesystem is still fully visible. `setup.sh` builds a fresh root at `$ROOT` (e.g. `/tmp/boid-root-XXXXXX`) and pivots into it.

The main steps:

- **bind mounts** — the kit's `additional_bindings`, the worktree, and stock system directories such as `/usr` and `/lib` are bind-mounted (or rbind-mounted) into `$ROOT`. This is what determines the file set visible inside the sandbox.
- **file writes** — kit and job-specific configuration files are materialised under `$ROOT`.
- **nftables rules** — drop outbound traffic except to the proxy port. Combined with pasta, this is what restricts where the sandbox can talk to.
- **symlinks** — the `boid` shim is symlinked at paths like `/opt/boid/bin/<command>` so the sandbox can invoke `boid` from inside.

`setup.sh` then re-runs `unshare` to pivot into the prepared root and launch `inner.sh`:

```bash
exec unshare --user --map-user=1000 --map-group=1000 --root="$ROOT" -- /bin/bash /tmp/inner.sh
```

- `--user` opens a new user namespace (rootless).
- `--map-user=1000 --map-group=1000` makes the sandbox's UID/GID 1000:1000.
- `--root=$ROOT` makes `$ROOT` the new root for the launched process.

From this point inside the sandbox:

- The home directory, SSH keys, and other projects do not exist (their paths simply do not resolve unless they were bind-mounted into `$ROOT`).
- The process runs as UID 1000 with no escalation path to the host's root.

### 3. `inner.sh`

Now we are inside the sandbox. `inner.sh` exports environment variables (`BOID_TASK_ID` and friends, including anything declared by the kit / behavior), then feeds the TaskJSON on stdin to the handler's argv. The handler's exit code propagates through `exec`, back through `setup.sh`, and out via `outer.sh`.

The handler-side protocol is documented in [Hook script protocol](../reference/hook-contract.md).

## Network control

Network containment has two layers.

### ① pasta (the network namespace)

pasta is a user-privilege network-namespace wrapper. The sandbox sees only pasta's virtual network; the host's physical NICs are invisible. Outbound traffic is relayed back to the host by pasta itself.

### ② nftables drop rules

`setup.sh` programmes nftables to drop all outbound traffic except to the proxy port. The end result:

- HTTP/HTTPS goes only through `http_proxy` / `https_proxy` pointing at `10.0.2.2:<port>`.
- The proxy forwards only those domains the kit declared in `kit.yaml`.
- Other TCP/UDP is blocked.

The proxy itself lives in [`internal/sandbox/proxy.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/proxy.go) and runs as a goroutine inside the daemon.

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

## Cleanup

Cleanup runs in **`outer.sh`** after pasta returns; `setup.sh` no longer installs a trap.

```bash
# outer.sh (excerpt)
...
exit_code=$?
...
rm -f "$pasta_stderr" 2>/dev/null || true
case "$root_dir" in
    /tmp/boid-root-*) rm -rf "$root_dir" 2>/dev/null || true ;;
    *) echo "[boid] WARNING: root_dir=$root_dir not under /tmp/boid-root-*, skipping cleanup" >&2 ;;
esac
rm -rf <staging-dirs...> 2>/dev/null || true
if [ "$exit_code" -eq 0 ]; then
    rm -f <outer.sh> <setup.sh> <inner.sh> 2>/dev/null || true
fi
exit $exit_code
```

Three points:

1. **The kernel reclaims mounts.** `setup.sh` runs inside `unshare --mount`; when its bash exits the namespace is destroyed and every bind underneath is reclaimed by the kernel. The previous implementation called `umount -R "$ROOT"` from a trap, which failed immediately because `$ROOT` itself was never a mountpoint, leaving every sub-bind alive and forcing the cleanup to skip `rm`.
2. **`$root_dir` is rm'd from outside the sandbox namespace.** By the time `outer.sh` resumes, the sandbox mount namespace is gone, so `rm -rf` only sees the empty scaffolding directory on the host. It cannot traverse a bind mount into host content.
3. **`/tmp/boid-root-*` prefix guard.** If `$root_dir` is set to an unexpected path (misconfiguration or bug), the case skips the rm and logs a warning instead.

On `exit_code != 0` the script files (`*-outer.sh`, `*-setup.sh`, `*-inner.sh`) are retained for post-mortem diagnosis (see `cleanupSandboxAfterWait` in `internal/dispatcher/runner.go`). `root_dir` and the staging dir are not retained — once the namespace is gone they are just empty scaffolding, so they are always removed.

A previous regression where bind mounts traversed during `rm -rf` deleted host files (memory: "feedback: bind_rm_traverses_source") motivated the current design — all three paths (own-namespace, cross-namespace, chroot-holder) now fail closed.

## Allowed boid builtins from inside the sandbox

Handlers running inside the sandbox (hook, gate, exec) can call two built-in commands: `boid` and `git`.
Both are injected automatically — no declaration in `project.yaml` / `kit.yaml` is needed.

### boid builtin

All roles (hook and gate) share the same allowed op set — there is no role branching.

| Op (sandbox protocol) | Corresponding CLI | Purpose |
|---|---|---|
| `job_done` | `boid job done <id>` | Notify the daemon that this job completed |
| `job_list` | `boid job list --task <id>` | List jobs belonging to a task |
| `job_show` | `boid job show <id>` | Show job detail |
| `job_log` | `boid job log <id>` | Retrieve job execution log |
| `action_send` | `boid action send` | Dispatch a manual action |
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

### git builtin

All roles share the same allowed op set.

| Op | Corresponding CLI | Purpose |
|---|---|---|
| `fetch` | `git fetch ...` | Fetch from remote |
| `push` | `git push ...` | Push to remote |
| `push_delete` | `git push origin --delete <branch>` | Delete a remote branch |

### Design notes

- **No role branching** — `boid` and `git` policies use `_ Role`; every role gets the same op set.
  Add a role `switch` inside `policyFor` only when a new builtin genuinely needs role-specific restrictions.
- **Source of truth** — `internal/orchestrator/policy.go`, functions `boidPolicy` / `gitPolicy`.
- **Sandbox-side enum** — `internal/sandbox/protocol.go`.
- **Cross-workspace access** is denied by the broker (`internal/sandbox/broker.go` `handleBoidBuiltin`)
  via `entry.Context.AllowsProject(...)` and similar guards — the op set above does not bypass these checks.

## Related documents

- [Architecture overview](overview.md) — where the sandbox layer sits.
- [Concepts / Sandbox](../guide/concepts.md#sandbox) — the user-visible meaning.
- [Hook script protocol](../reference/hook-contract.md) — the I/O contract for handlers running inside.
- [`project.yaml` reference](../reference/project-yaml.md) — declaring `host_commands` / `additional_bindings` / `capabilities`.
- [Docker proxy migration guide](../guide/docker-proxy-migration.md) — migrating from the docker kit (cetusguard) to the native proxy.
