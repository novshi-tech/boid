---
name: boid-executor
description: Runs an executor task (writable implementation) for the boid orchestrator.
  Reads task.yaml + instructions.yaml, makes the requested change inside its
  writable worktree, commits, and exits. The release step (commit-only, push,
  PR creation, etc.) is specified by the active instruction.
---

# boid Executor

An executor task **implements** what the parent supervisor asked for. It runs inside a writable worktree (provisioned by the project-top `worktree: true` setting in `.boid/project.yaml`), edits files, runs tests, and finishes with a commit. The parent supervisor handles integration — the executor only commits and exits.

## Context to Read First

| File | Contents |
|---|---|
| `~/.boid/context/task.yaml` | Title, description, status, behavior |
| `~/.boid/context/instructions.yaml` | Instructions array; **the last element is active** |
| `~/.boid/context/payload.yaml` | Existing artifacts (parent context, prior results) |
| `~/.boid/context/environment.yaml` | Sandbox constraints (RO/RW, network, tools) |

The **active instruction** (last element of `instructions.yaml`) carries the project-specific release step — e.g. plain `git add` + `git commit` (local merge model), or invoking a project release skill such as `/dev-pr-flow` (PR model). This skill describes only the generic implement-and-commit loop; the release details come from the instruction.

When the task has been reopened, earlier elements of `instructions.yaml` are prior context only — act on the tail.

## Workflow

1. **Read** — title + description in `task.yaml`, the active instruction at the tail of `instructions.yaml`. Confirm the worktree path and writability via `environment.yaml`.
2. **Implement** — make the code / test / doc changes the task asks for. Stay inside the executor's worktree.
3. **Verify** — run the project's quick verification (typically tests + lint) before committing. The active instruction usually names the verification command or release skill that includes verification.
4. **Release** — follow the release steps in the active instruction. Common shapes: `git add` + `git commit` (+ optional `git push`), or invoking a project release skill.
5. **Exit** — run `boid agent stop "$BOID_JOB_ID"`. The daemon delivers SIGUSR1 to the runtime; `run-agent.py` catches it and SIGTERMs only the `claude` process, leaving bash and the EXIT trap alive. The trap fires `boid job done --output-file payload_patch.json`, which is the canonical CompleteJob call — it transports the agent's session id and any artifact written to `payload_patch.json` to the daemon through a still-valid broker token. The task then transitions to `done` and the parent supervisor takes it from there.

> Note: executor agents run in interactive PTY sessions (the `claude` binary does not exit on its own), so the EXIT trap alone is not enough — you must call `boid agent stop` explicitly. Do **not** call `boid job done` from the agent: that would unregister the broker token before the EXIT trap runs and silently drop the agent's `payload_patch.json` (session id, artifact).
>
> Safety net: the claude-code kit injects a `Stop` hook via `--settings` that calls `boid agent stop "$BOID_JOB_ID"` whenever your response loop ends, so a forgotten exit call will not strand the task in `executing`. Still call `boid agent stop` explicitly — the Stop hook is a backstop, not the contract.

## Asking the User

When the work cannot proceed (missing fact, ambiguous spec), pause and ask:

```bash
boid task notify "$BOID_TASK_ID" \
  --message "<short summary for the push notification>" \
  --ask "<full question body>"
```

Both `--message` (short) and `--ask` (full body) are required. The call transitions the task to `awaiting`, fires the notify hook, and the daemon then SIGTERMs your runtime — **just stop generating after the call returns**. No sentinel output, no explicit exit. When the user replies, the kit re-invokes you with `$BOID_USER_ANSWER` set and your prior `claude --resume` session id restored. Branch on the answer to continue.

## Aborting

For unrecoverable problems (broken sandbox, prerequisites the user cannot fix), abort instead of exiting normally:

```bash
boid action send --task "$BOID_TASK_ID" --type abort \
  --payload '{"code":"<short-code>","message":"<summary>"}'
```

This transitions the task to `aborted` and records the reason in `lifecycle.abort.message`. The parent supervisor sees the abort and decides whether to retry or escalate to the user.

## Progress Reporting

For long-running work, leave a progress note on the task timeline:

```bash
boid task notify "$BOID_TASK_ID" --progress "<message>"
```

No state change, no notify hook — just a timeline entry visible in the Web UI / TUI. Useful for multi-hour implementations.

## Rules

- Only edit files inside your worktree (path in `environment.yaml`). Anything written outside is lost when the worktree is removed on `done`.
- Follow constraints in `environment.yaml` (`network.restricted`, `tools`).
- **Always commit before exiting.** Uncommitted changes vanish with the worktree.
- Do not spawn child tasks. Triage and decomposition belong to the supervisor; executors implement.
- Do not write to `instructions` in the task payload — it is delivered as the read-only file `instructions.yaml`.
- On reopen, the new instruction is appended as the last element of `instructions.yaml`. Read the new tail; earlier elements are context.
