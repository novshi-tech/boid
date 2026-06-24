# boid Built-in Command Reference

These `boid` subcommands are available inside the supervisor's sandbox via the shim. The shim talks to the boid daemon over the host bridge; direct daemon access is not required.

## Contents

- [boid task create](#boid-task-create)
- [boid task show](#boid-task-show)
- [boid task list](#boid-task-list)
- [boid task notify](#boid-task-notify)
- [boid task reopen](#boid-task-reopen)
- [boid task update](#boid-task-update)
- [boid task ask](#boid-task-ask)
- [boid task answer](#boid-task-answer)
- [boid task delete](#boid-task-delete)
- [boid task import](#boid-task-import)
- [boid action send](#boid-action-send)
- [boid agent stop](#boid-agent-stop)
- [boid job done](#boid-job-done)
- [boid job list](#boid-job-list)
- [boid job show](#boid-job-show)
- [boid job log](#boid-job-log)

## boid task create

```bash
boid task create <<YAML
title: <required>
behavior: executor
description: |
  ...
auto_start: true
YAML
```

Reads YAML/JSON from stdin (or `-f <file>`). Prints `task created: <id> (<status>)`.

| Field | Description |
|---|---|
| `title` | Required. |
| `behavior` | `executor` / `supervisor` (default: `supervisor`). Aliases `plan` / `dev` are accepted. |
| `description` | Delivered to the child as the active instruction. |
| `auto_start` | `true` to start immediately. |
| `parent_id` | Auto-filled from `BOID_TASK_ID`. Set explicitly only to attach under a different parent. |
| `base_branch` | Branch to fork the worktree from. Inherits the project-top `base_branch`, else the daemon's current HEAD. |
| `project_id` | Project to create in. Defaults to the same project as the parent. |
| `behavior_spec` | Inline behavior definition. Not normally needed — prefer named behaviors from `project.yaml`. |
| `ref` | Child ref name within the parent scope. |

## boid task show

```bash
boid task show <task-id> --field <path>
```

Inside the sandbox, `--field` is required and the value is printed as plain text. The `<path>` is a dotted JSON path that resolves first against top-level Task fields and then falls back to the payload (so payload traits work without an explicit `payload.` prefix). Common paths:

- `status` — current TaskStatus (`pending` / `executing` / `awaiting` / `done` / `aborted`).
- `title`, `description`, `behavior`, `parent_id` — task metadata.
- `awaiting.question`, `awaiting.question_id` — question text and turn id when a child is in `awaiting`.
- `artifact.<key>` / `payload.artifact.<key>` — values written by the child into its payload.
- `lifecycle.abort.message`, `lifecycle.executed` — computed lifecycle (derived from action history; does not require persisted payload).
- `payload` — whole payload as compact JSON (pipe through `jq` for further extraction).

## boid task list

```bash
boid task list [--status <s>] [--workspace <ws>] [--behavior <name>]
               [--has-depends-on | --no-depends-on]
```

Lists tasks in the supervisor's workspace. The broker enforces scope.

## boid task notify

```bash
boid task notify <task-id> \
  --message "<short>" \
  [--ask "<question body>" | --progress "<note>"] \
  [--question-id <id>]
```

Three modes:

| Mode | Flags | Behavior |
|---|---|---|
| FYI | `--message` only | Fires the notify hook. No state change. Use for milestone signals only. |
| Ask | `--message` + `--ask` | Fires the hook and transitions to `awaiting`. The daemon **no longer dispatches a resume hook** when the user answers — the legacy session-resume path was removed. For real Q&A use `boid task ask` (blocking RPC) instead. |
| Progress | `--progress` only | Records a timeline action. **No hook fires, no state change.** Mutually exclusive with `--ask`. |

`--question-id` is auto-generated when omitted. Provide it only if you need a stable correlation key across asks.

## boid task reopen

```bash
boid task reopen <task-id> [-m "<new instruction>"]
```

Transitions `done → executing` with the new instruction appended to `instructions.yaml`. Used by the supervisor to ask a `done` child to take another pass (rebase, fix CI, address review feedback). The same task ID and worktree are reused.

Reopen always spawns a **fresh agent process** — the harness session from the
previous turn is not carried over (no `claude --resume` / `codex exec resume` /
`opencode run -s ... --continue`). The reopened agent re-reads
`~/.boid/context/{task,instructions,payload}.yaml` on cold start, so the new
instruction landing in `instructions.yaml` plus any prior artifact in
`payload.yaml` is the full context surface.

Aborted tasks cannot be reopened.

## boid task update

```bash
boid task update <task-id> --patch-file <file>             # task row fields
boid task update <task-id> --payload-file <file>           # merge into task.payload
boid task update <task-id> --instructions-file <file>      # role-wise instructions merge
```

`-` reads from stdin. Use `--payload-file` to seed children with structured context; prefer `reopen` for adding plain-text instructions.

## boid task ask

```bash
ANSWER=$(boid task ask "<question body>")
```

Harness-independent **blocking** Q&A. The question is a single positional argument (multiple positional args are space-joined). **Flags are rejected** — there is no `--question` or `--question-id` form; the question id is auto-generated by the daemon and surfaced in `payload.awaiting.question_id`.

The call holds the connection open: your turn stays alive, the task transitions to `awaiting`, and the answer arrives on stdout when the user/supervisor replies via `boid task answer`. No `$BOID_USER_ANSWER`, no session resume, no second hook dispatch — the same hook job goes `running → (parked) → running → completed`. Works under any harness (claude-code, codex, opencode, ...).

- TaskID is **not** passed: the broker fills it from the calling agent's token, so `boid task ask` always targets the caller's own task.
- boid never times the call out (it waits indefinitely). Your **harness** does, though: it kills any single shell command after its command-timeout (~2 min on claude-code/opencode). When that happens, **re-run the identical `boid task ask`** — it re-attaches to the same pending question, or returns the answer immediately if it already arrived while you were disconnected. The task stays `awaiting` across retries and the answer is persisted, so nothing is lost. Keep retrying until it prints the reply.
- Re-asking the **same** question is the retry above and is always allowed. Only a **different** `boid task ask` while one is still pending fails immediately with `task_ask: another question is pending`.
- The user/parent answers with `boid task answer` (see below).

`boid task ask` is the only supported Q&A path. `notify --ask` still transitions the task to `awaiting` for compatibility, but the daemon no longer dispatches a resume hook on answer — the agent has already exited and there is nowhere for the reply to land.

## boid task answer

```bash
boid task answer --task <id> --question-id <id> --answer <text>
```

Posts a user reply to an `awaiting` task. All three flags are required. Normally the user replies via the Web UI / push hook; supervisors rarely call this directly. The answer is delivered only via the blocking RPC path (`boid task ask`); a `notify --ask` awaiting that was never paired with a parked broker connection cannot be resolved this way (`409 Conflict: no agent is waiting for this answer`).

## boid task delete

```bash
boid task delete <task-id> [--force]
```

`--force` is required for non-terminal tasks. Supervisors should prefer aborting a live child (see `boid action send --type abort`) over deleting it.

## boid task import

```bash
boid task import [-f <file>] [--project <id>]
```

Bulk-imports tasks from a JSONL stream. Used by data-source integrations (Jira, To Do, etc.); supervisors generally do not need it.

## boid action send

```bash
boid action send --task <id> --type <action> [--payload <json|@file>]
```

Submits a manual state-machine action against the task (see [state-machine.md](state-machine.md)). The supervisor uses this when it needs to forcibly transition a child (rare):

- `--type abort` — abort a live child. Pass a JSON payload like `{"code":"...","message":"..."}` to record the reason.
- `--type start` — start a pending child (normally handled by `auto_start: true`).

Most workflows go through the dedicated subcommands (`reopen`, `notify`, etc.) and never need `action send`.

## boid agent stop

```bash
boid agent stop <job-id>
```

Canonical "I'm done, please end my session" call for interactive agents (supervisor / executor / plan). Use this for the autonomous exit path: `boid agent stop "$BOID_JOB_ID"`.

The daemon delivers SIGUSR1 to the runtime's process group. `run-agent.py` catches it and SIGTERMs only the `claude` process; bash and the EXIT trap survive (`trap '' USR1` is inherited as SIG_IGN). The trap then fires `boid job done --output-file payload_patch.json` against a still-valid broker token, completing the job through the normal path with the agent's session id (and any artifact written to `payload_patch.json`) intact.

> Why not call `boid job done` directly? `CompleteJob` unregisters the broker token immediately, so the bash EXIT trap's follow-up `boid job done --output-file ...` would be silently rejected as `invalid token` — dropping any payload patch the agent wrote. Always go through `agent stop`.

## boid job done

```bash
boid job done <job-id> --exit-code <n> [--output-file <path>]
```

Low-level CompleteJob call. The bash EXIT trap fires this automatically with `--output-file payload_patch.json` after `boid agent stop` (or after `notify --ask`). Agents normally do not invoke `boid job done` themselves — use `boid agent stop` instead.

## boid job list

```bash
boid job list --task <task-id>
```

Lists all jobs (hook invocations) for the task. Primary use: diagnosing aborts.

## boid job show

```bash
boid job show <job-id>
```

Shows job role, exit code, status, and timestamps.

## boid job log

```bash
boid job log <job-id>
```

Streams the runtime transcript for a job. Prints `log not available (runtime cleaned up)` once the daemon GC has removed the runtime (24h cycle, 30-day retention).
