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

You likely re-ran `go install` but forgot to restart the daemon. The running daemon has the **old** binary mapped into memory, even though `which boid` points at the new one.

Diagnose:

```bash
# If the running boid binary on disk has been replaced, /proc/<pid>/exe will
# show "(deleted)".
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

1. **The hook has no exit path.** A hook that is blocking on a prompt, an interactive command, or a hung agent will keep the dispatch loop waiting. `boid job list --task <id>` will show a `running` job that never finishes. Run `boid task abort <id>` to clean up, then look at the hook script.
2. **The payload never gets the trait that signals completion.** Without `artifact` (or `tasks` for plan tasks), the executing-state auto-transition rules cannot fire. Inspect the payload with `boid task show <id>` and check that the hook is emitting a payload patch with the expected trait.
3. **A `verifying`-sourced finding is still open.** If the task bounced back from `verifying`, an unresolved finding can keep it in `reworking`. `boid task get <id> findings` (or `task show`) will list them.

## A task is stuck in `reworking` forever

Same logic as above but specifically for `reworking → verifying`: the rework hook needs to clear all `reworking`-sourced findings. If the hook keeps writing new ones, you will hit the rework-limit auto-abort eventually (`code=rework_limit_exceeded`). Raise the limit via `state_machine.rework_limit` in `~/.config/boid/config.yaml` if 5 is genuinely too low for your workflow, but more often a stuck rework loop is a real problem with the rework hook.

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

Hooks run inside a sandbox. If the script tries to run a command that the kit's `host_commands` does not allow, the sandbox blocks it. Two paths to fix:

- Add the missing command to the kit's `host_commands` list (preferable for general-purpose tools like `git push`).
- Move the responsibility to a gate, which runs on the host without a sandbox (preferable for environment-specific operations like `systemctl restart`).

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
