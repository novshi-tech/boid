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
2. **Signal upward** via `boid task notify` with the appropriate flag — `--done` (success), `--fail` (failure), or `--ask` (mid-flight question). The flag chooses the state transition: `done`, `aborted`, or `awaiting` respectively. Same contract for root and child executors.

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

After writing your report, emit exactly one of:

- **Complete** — your task is done. Transitions to `done`:
  ```bash
  boid task notify "$BOID_TASK_ID" \
    --message "<short summary>" \
    --done "<one-line achievement>"
  ```
- **Failure** — you cannot complete (let your supervisor decide reopen / leave aborted). Transitions to `aborted`:
  ```bash
  boid task notify "$BOID_TASK_ID" \
    --message "<short summary>" \
    --fail "<what went wrong, what you tried>"
  ```
- **Decision needed** — mid-flight question for your owner. Transitions to `awaiting`:
  ```bash
  boid task notify "$BOID_TASK_ID" \
    --message "<short summary>" \
    --ask "<question>"
  ```

The contract is identical for child (`parent_id != ""`) and root (`parent_id == ""`) executors. After the call returns, the daemon SIGTERMs your runtime — **just stop generating**. The bash EXIT trap fires `boid job done` to seal your session id. For root tasks the user-facing notify hook also fires (desktop notification with deep-link to the task page); for child tasks the parent supervisor's polling picks up the new status.

> Do **not** call `boid job done` or `boid agent stop` directly. The first unregisters the broker token before the EXIT trap runs (silently dropping your `payload_patch.json`); the second is a legacy escape hatch superseded by `notify --done` / `--fail` / `--ask` which give your owner an explicit state signal.

> No exit safety net: the claude-code kit no longer auto-fires `boid agent stop` when your response loop ends (the Stop hook was removed in lifecycle-accountability Phase 2.a). Executors that end with bare assistant text leave the task stuck in `executing` — **always emit a `notify --done` / `--fail` / `--ask` call explicitly** before ending the turn.

## Asking Your Owner

When the work cannot proceed (missing fact, ambiguous spec), pause and ask:

```bash
boid task notify "$BOID_TASK_ID" \
  --message "<short summary for the push notification>" \
  --ask "<full question body>"
```

The daemon transitions the task to `awaiting`. For child executors, the supervisor's polling picks up the question. For root executors, the user is notified directly. **Just stop generating after the call returns** — no sentinel, no explicit exit. The kit re-invokes you with `$BOID_USER_ANSWER` set and your `claude --resume` session id restored. Branch on the answer to continue.

## Aborting vs Failing

Two transitions land in `aborted`, with different intent:

- `boid task notify --fail "<message>"` — agent self-reports failure with a structured report on `payload.artifact.report`. **Default for any failure**: lets the supervisor read the report and decide reopen / accept.
- `boid action send --task "$BOID_TASK_ID" --type abort --payload '{"code":"<code>","message":"<summary>"}'` — terminal abort with no payload to surface. Use only for situations where the executor has no useful information (broken sandbox at startup, missing prerequisites). The supervisor still sees the abort but has nothing to verify.

Prefer `--fail`. Use `action send --type abort` only when truly nothing can be done from the executor side.

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
