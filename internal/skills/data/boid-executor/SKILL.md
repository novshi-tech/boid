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

## Lifecycle Accountability

Your **owner** is the parent supervisor (`parent_id != ""`) or the user (`parent_id == ""`). Before exiting, you must:

1. **Write your structured report** to `payload.artifact.report` — your owner reads it as the canonical source for your work product
2. **Signal upward** via `boid task notify --ask "done_request: ..."` (or `failure_report: ...`) — unless you are a root executor (`parent_id == ""`), in which case the existing `boid agent stop` flow applies

Children that exit silently without notification leave the task in `executing` with no signal to the owner. The supervisor's polling can detect that as a stuck child, but **explicit notification is the contract**.

See [docs/plans/lifecycle-accountability.md](../../../docs/plans/lifecycle-accountability.md) for the full design.

## Workflow

1. **Read** — title + description in `task.yaml`, the active instruction at the tail of `instructions.yaml`. Confirm the worktree path and writability via `environment.yaml`.
2. **Implement** — make the code / test / doc changes the task asks for. Stay inside the executor's worktree.
3. **Verify** — run the project's quick verification (typically tests + lint) before committing. The active instruction usually names the verification command or release skill that includes verification.
4. **Release** — follow the release steps in the active instruction. Common shapes: `git add` + `git commit` (+ optional `git push`), or invoking a project release skill.
5. **Report & Exit** — Write `payload.artifact.report` (see [Writing the Final Report](#writing-the-final-report)), then signal your owner (see [Exit Handling](#exit-handling)).

## Writing the Final Report

Before signalling your owner, persist your structured report:

```bash
boid task update "$BOID_TASK_ID" --payload-file - <<EOF
artifact:
  report:
    summary: "<1-3 lines: what you did>"
    evidence:
      pr_url: "<if applicable>"
      commit_sha: "<if applicable>"
      worktree_branch: "<branch name, if applicable>"
    verification:
      tests_passed: true
      ci_status: "<green|red|pending|unknown>"
      manual_checks: ["<...>"]
    caveats: ["<known limitations, untested aspects>"]
    open_questions: ["<things you want the owner to decide>"]
EOF
```

`summary` is required; other fields are optional but encouraged when relevant. The owner (parent supervisor or user) reads this as the **canonical Layer A source** for your work product; if it is empty or missing, the supervisor treats that as a missing-report anomaly and reopens you.

## Exit Handling

Check your owner first:

```bash
parent=$(boid task show "$BOID_TASK_ID" --field parent_id)
```

### Child executor (`parent_id != ""`)

After writing your report, emit exactly one of:

- **Complete** — your task is done:
  ```bash
  boid task notify "$BOID_TASK_ID" \
    --message "<short summary>" \
    --ask "done_request: <one-line achievement>"
  ```
- **Failure** — you cannot complete (let your supervisor decide reopen / abort):
  ```bash
  boid task notify "$BOID_TASK_ID" \
    --message "<short summary>" \
    --ask "failure_report: <what went wrong, what you tried>"
  ```
- **Decision needed** — mid-flight question for your supervisor:
  ```bash
  boid task notify "$BOID_TASK_ID" \
    --message "<short summary>" \
    --ask "<question>"
  ```

After the call returns, the daemon SIGTERMs your runtime — **just stop generating**. The bash EXIT trap fires `boid job done` to seal your session id. Do **not** call `boid agent stop` when you have a parent — the supervisor never sees the silent termination (the anti-pattern that motivated this model).

### Root executor (`parent_id == ""`)

You are an autonomous dev task with no supervisor. Use the existing pattern:

```bash
boid agent stop "$BOID_JOB_ID"
```

The daemon delivers SIGUSR1 to the runtime; `run-agent.py` SIGTERMs only the `claude` process. The bash EXIT trap fires `boid job done --output-file payload_patch.json`, transporting your session id and any artifact through the still-valid broker token. The task auto-advances to `done`.

> Note: do **not** call `boid job done` directly from the agent — that would unregister the broker token before the EXIT trap runs and silently drop your `payload_patch.json`.

> Safety net: the claude-code kit registers a `Stop` hook that calls `boid agent stop` whenever your response loop ends. For child executors this fallback is the **wrong behavior** (silently terminates without signalling the supervisor); always emit explicit `notify --ask` first. The Stop hook is scheduled for removal in lifecycle-accountability Phase 2.

## Asking Your Owner

When the work cannot proceed (missing fact, ambiguous spec), pause and ask. Use the same `notify --ask` mechanism as exit, but with a question (no prefix):

```bash
boid task notify "$BOID_TASK_ID" \
  --message "<short summary for the push notification>" \
  --ask "<full question body>"
```

The daemon transitions the task to `awaiting`. For child executors, the supervisor's polling picks up the question. For root executors, the user is notified directly. **Just stop generating after the call returns** — no sentinel, no explicit exit. The kit re-invokes you with `$BOID_USER_ANSWER` set and your `claude --resume` session id restored. Branch on the answer to continue.

## Aborting (terminal, no parent decision)

For unrecoverable problems where you have **no useful information to surface** to your owner (broken sandbox at startup, missing prerequisites), abort directly:

```bash
boid action send --task "$BOID_TASK_ID" --type abort \
  --payload '{"code":"<short-code>","message":"<summary>"}'
```

This transitions the task to `aborted` immediately. The parent supervisor sees the abort and decides whether to retry or escalate.

Prefer `failure_report:` via `notify --ask` (above) when the situation might be recoverable — that lets your supervisor decide between reopen / abort / escalate. Use `action send --type abort` only when truly nothing can be done from the executor side.

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
