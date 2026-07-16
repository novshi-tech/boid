# Troubleshooting

A short list of the things that have caught the maintainers themselves. If you hit something not on this list, the daemon log (`~/.local/state/boid/boid.log`) and `boid task show <id>` together usually pinpoint the issue.

## The daemon will not start

```text
Error: boid server already running (socket: /run/user/1000/boid.sock)
```

Another `boid` process is already listening. Use `boid stop` (not `kill`) to bring it down cleanly.

If `boid stop` reports "no server running" but you still see this error, the socket is stale and a previous daemon left it behind. Remove it manually:

```bash
rm "$XDG_RUNTIME_DIR/boid.sock"
```

## A bug fix I just installed has no effect

You likely re-ran `go install` but forgot to restart the daemon. Even though the binary on disk is now the new one, the daemon process is still running the code it loaded at startup, which stays resident in memory until the process exits.

Diagnose:

```bash
# If /proc/<pid>/exe shows "(deleted)", the binary that was loaded at startup
# is no longer on disk — i.e. the new install replaced it but the old daemon
# is still running.
ps -o pid,cmd -C boid
ls -l /proc/<pid>/exe
```

Fix:

```bash
boid stop
boid start
```

This is the single most common reason "I fixed it but it still happens" — when in doubt, restart.

## A task is stuck in `executing` forever

**The hook is not finishing.** A hook blocked on a prompt, on an interactive command that never returns, or on an unresponsive agent leaves the daemon waiting for the job to complete. `boid job list --task <id>` will show a job stuck in `running`. Run `boid action send --task <id> --type abort` to release it, then inspect the hook script.

## `boid task list` is slow / disk fills up

Local data accumulates in two places:

| Path | Owned by | Auto-GC |
|---|---|---|
| `~/.local/share/boid/runtimes/<id>/` | `boid` | Yes (every 24h, removes >30d) |
| `~/.claude/projects/-workspace-<project-name>-.../` (per project) | Claude Code | **No** — manual cleanup only |

The first is GC'd automatically. The second is written by Claude Code itself; `boid` does not touch it.

> **Note (change after the git gateway cutover)**: before the cutover, each task's cwd was its per-task host git worktree path (`-home-...-worktrees-boid-<taskid>-` shape), so session logs were split by task. After the cutover, the job cwd is the sandbox-internal `/workspace/<project-name>` (project-name based, no task ID), so **multiple tasks in the same project now aggregate into the same session-log directory**. Dispatch one task once to see the actual directory name in `~/.claude/projects/`. If that directory bloats, verify no other-project entries would be caught in the wildcard and then delete the specific directory manually.

GC settings live in `~/.config/boid/config.yaml`:

```yaml
gc:
  enabled: true
  interval: 24h
  older_than: 720h    # 30 days
```

**What the daemon GC actually removes:** In addition to `runtimes/<runtime_id>/` directories, the GC pass also cleans up: terminal tasks/actions/jobs from the database, `/tmp/boid-*` temporary files, and revoked devices. The first GC run happens **10 seconds after daemon start**, not immediately on startup.

## "permission denied" or "unknown command" inside a hook

Hooks run inside a sandbox, so any command name not present in the project's assigned **workspace**'s `host_commands` is rejected. `host_commands` is a two-tier structure: the workspace only carries a list of reference **names** — the actual definitions (`path`/`allow`/`deny`/`env`) live in the daemon-wide registry `~/.config/boid/host_commands.yaml`. If a command is missing, you may need either or both of:

- `boid workspace edit <slug> --from-file <yaml>` to add the name to the workspace's `host_commands` list
- If the name isn't in the daemon-wide registry yet, add it to `~/.config/boid/host_commands.yaml` and run `boid host-commands reload` (see [Onboarding / Defining host_commands](onboarding.md#defining-host_commands-the-daemon-wide-registry))

(This is how `git push`, `gh`, and similar tools are exposed to hooks.)

## Web UI: the device keeps getting logged out

The cookie is `HttpOnly; Secure; SameSite=Lax`. If your phone's browser is configured to clear cookies on close, the device login won't survive. Use a different browser or disable that policy for the host.

If the public URL was changed after pairing, magic links from notifications will go to the old URL. Re-run `boid web set-url <new-url>`.

## Web UI: pairing code says "expired" or "invalid"

Codes expire 5 minutes after issue and are single-use. Run `boid web pair` again.

## I see `(deleted)` in `/proc/<pid>/exe` for boid

You re-installed the binary on disk but the daemon is still running the old code. See "[A bug fix I just installed has no effect](#a-bug-fix-i-just-installed-has-no-effect)".

## Where to look first

- **Daemon log**: `~/.local/state/boid/boid.log` (rotated)
- **Task state**: `boid task show <id>`
- **Job logs**: `boid job show <id>`
- **Live updates**: `boid task watch <id>` or the Web UI task detail page

If something looks like a bug, file an issue with the task/job IDs and a snippet of the daemon log around the failure.
