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
- [`project.yaml` reference](../reference/project-yaml.md) — declaring `host_commands` / `additional_bindings` / `env`.
