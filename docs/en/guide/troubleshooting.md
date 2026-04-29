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

Three possibilities:

1. **The hook (the executing-state script) is not finishing.** A hook blocked on a prompt, on an interactive command that never returns, or on an unresponsive agent leaves the daemon waiting for the job to complete. `boid job list --task <id>` will show a job stuck in `running`. Run `boid task abort <id>` to release it, then inspect the hook script.
2. **The completion-signaling trait was never written to the payload.** Without `artifact` (or `tasks` for plan-style tasks), the `executing` auto-transition rules never fire. Use `boid task show <id>` to inspect the payload and verify the hook is emitting a payload patch that includes the expected trait.
3. **An open finding sourced from `verifying` is still around.** Once a task has visited `verifying`, an unresolved finding keeps it pinned in `reworking`. Look at `verification.findings` in `boid task show <id>`.

## A task is stuck in `reworking` forever

Same logic as above but for the `reworking → verifying` direction. The rework-style hook running in `reworking` needs to flip every `reworking`-sourced finding to `resolved` to escape. If the hook keeps writing new findings, the task eventually hits the rework-count limit and aborts with `code=rework_limit_exceeded`. Raise `state_machine.rework_limit` in `~/.config/boid/config.yaml` if your workflow genuinely needs more than 5 cycles, but in most cases the real fix is in the rework-style hook itself.

## `boid task list` is slow / disk fills up

Local data accumulates in two places:

| Path | Owned by | Auto-GC |
|---|---|---|
| `~/.local/share/boid/runtimes/<id>/` | `boid` | Yes (every 24h, removes >30d) |
| `~/.claude/projects/-home-...-worktrees-boid-<taskid>/` | Claude Code | **No** — manual cleanup only |

The first is GC'd automatically. The second is written by Claude Code itself; `boid` does not touch it. If your `~/.claude/projects/` is huge, clean it manually:

```bash
rm -rf ~/.claude/projects/-home-*-worktrees-boid-*
```

(Be careful — there are likely entries for other projects that should not be deleted.)

GC settings live in `~/.config/boid/config.yaml`:

```yaml
gc:
  enabled: true
  interval: 24h
  older_than: 720h    # 30 days
```

## "permission denied" or "unknown command" inside a hook

Hooks run inside a sandbox, so any command the kit has not declared in `host_commands` is rejected. Two ways to fix it:

- Add the missing command to the kit's `host_commands` list (preferred for general-purpose tools like `git push`).
- Move the work to a gate (a script that runs on the host at a state transition) — preferred for environment-specific operations like `systemctl restart`.

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
