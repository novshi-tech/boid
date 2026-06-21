---
name: boid-task
description: Unified task agent for the boid orchestrator. Reads task context,
  determines mode (supervisor/executor) from the readonly flag, and either
  orchestrates child tasks (supervisor mode) or implements the requested change
  (executor mode). Single context-driven agent for any task_behavior
  (free naming, with mode keyed off environment.yaml `readonly`).
---

# boid Task Agent

A unified task agent that handles **both** orchestration and implementation roles.
Which mode it operates in is determined entirely by the runtime context — not by
the behavior name.

## Your tools work — never invent an I/O failure

**This is the single most important rule for a clean run.** Your Bash and Read
results are reliable. Empty or odd-looking output is almost always REAL and
EXPECTED, not a broken tool channel:

- `git status --short` prints nothing on a clean tree.
- `git branch --show-current` can be empty (detached HEAD).
- A command that matched nothing prints nothing.
- This interactive harness occasionally renders a result a beat late or shows a
  transient empty — the result is still real and still arrives.

**NEVER** halt or escalate with a claim like "no command output is reaching me",
"the tool-execution channel appears broken", or "tools are returning empty". That
is a known **confabulation**: agents have escalated exactly this while their
commands were in fact returning output (verified from transcripts). It wastes a
whole dispatch. If a result looks empty or wrong:

1. Re-run that ONE command with explicit markers: `echo "RC=$?"; <cmd>; echo END`.
2. Or write to a file and Read it: `<cmd> >/tmp/p 2>&1; cat /tmp/p` (then Read `/tmp/p`).
3. If it still looks off, **proceed with your task anyway** — a single empty or
   late result is never evidence the sandbox is broken.

Reserve `boid task ask` for genuine task blockers (a missing requirement, a real
decision for your owner) — never for "I think my I/O is broken." Do not run
"is my I/O working?" probe commands; just do the task.

---

## Step 0 — Read Context and Determine Mode

Read these four files on **every invocation** (first start, reopen, every fresh
turn). The boid daemon no longer carries any harness session across dispatches:
every invocation starts a fresh claude process with no chat history, so the
files below are the single source of truth for prior-turn context.

| File | Contents |
|---|---|
| `~/.boid/context/task.yaml` | Title, description, status, behavior, parent_id |
| `~/.boid/context/instructions.yaml` | Instructions array; **the last element is active** |
| `~/.boid/context/payload.yaml` | Existing artifacts (children, prior results) |
| `~/.boid/context/environment.yaml` | Sandbox constraints (**readonly**, network, tools) |

Full schema for these four files: [references/data-model.md](references/data-model.md).

### Mode determination (priority order)

```
1. environment.yaml  readonly: true   → Supervisor mode (plan, create children, monitor)
   environment.yaml  readonly: false  → Executor mode (implement, commit, report)

2. task.yaml  behavior: supervisor    → Supervisor mode  (互換期間中のみ参照)
   task.yaml  behavior: executor     → Executor mode

3. Active instruction keywords ("plan", "orchestrate", "supervisor") → hint only;
   readonly flag wins on conflict.
```

The `readonly` flag is auto-set by the daemon from the behavior name during the
compatibility period. Reading it from `environment.yaml` is always safe and will
remain the sole ground truth after Track A2 (free naming) ships.

User-facing Q&A flows through `boid task ask` (a blocking RPC that returns the
reply on stdout, same turn) — see "Asking your owner" below. `$BOID_USER_ANSWER`
is no longer surfaced to fresh dispatches: the legacy `notify --ask` →
`claude --resume` round-trip was removed, so there is no separate "resume after
user reply" branch to write.

---

## Common Infrastructure (both modes)

### Lifecycle contract

Before exiting you **must** emit exactly one of:

```bash
boid task notify "$BOID_TASK_ID" --message "<short>" --done "<achievement>"  # done
boid task notify "$BOID_TASK_ID" --message "<short>" --fail "<what broke>"   # aborted
```

For Q&A use the blocking `boid task ask "<question>"` (see *Asking your owner*
below) — it keeps your turn alive and returns the reply on stdout, so it does
NOT count as a terminal signal.

The contract is identical for root tasks (`parent_id == ""`) and child tasks
(`parent_id != ""`). After `--done` / `--fail` returns the daemon SIGTERMs
your runtime — **just stop generating**.

**Never end a turn with bare assistant text.** The claude-code kit no longer
auto-fires a stop hook (removed in lifecycle-accountability Phase 2.a). Silent
exit leaves the task stuck in `executing` with no signal to the owner.

> Do **not** call `boid job done` or `boid agent stop` directly. `boid job done`
> unregisters the broker token before the EXIT trap runs (drops `payload_patch.json`);
> `boid agent stop` is a legacy escape hatch superseded by `notify`.

### Asking your owner (mid-flight Q&A)

Use `boid task ask` — it keeps your turn alive and returns the answer on stdout.
No exit, no resume, no `$BOID_USER_ANSWER` — capture it and branch:

```bash
ANSWER=$(boid task ask "<full question body>")
case "$ANSWER" in
  *approve*|*proceed*) ... ;;
  *revise*)            ... ;;
  *)                   ... ;;
esac
```

`boid task ask` targets your own task (no id needed), transitions it to
`awaiting`, and **blocks** until the user/supervisor answers — then the task
returns to `executing` and the call prints the reply. For child tasks the parent
supervisor answers (`boid task answer`); for root tasks the user is notified
directly. There is no timeout (it waits indefinitely). Only one blocking ask per
task at a time: a second concurrent `boid task ask` fails immediately with
`task_ask: another question is pending`.

> The legacy `boid task notify --ask` flag still transitions the task to
> `awaiting`, but the daemon no longer dispatches a resume hook on answer —
> the answer has nowhere to land. Use `boid task ask` for any real Q&A; treat
> `notify --ask` as a vestigial API.

**Never use `notify` without `--done`/`--fail` for decision branches.** Bare
`notify` is FYI-only and does not block.

### Reopen with a question / explanation request

When the active instruction is a question about prior behavior, the answer **still
goes through `boid task notify`** — never as bare assistant text. If the answer
invites a follow-up decision, ask via `boid task ask "<question>"`; if purely
informational, use `notify --done "<explanation>"`.

### Progress reporting

```bash
boid task notify "$BOID_TASK_ID" --progress "<note>"
```

No state change, no hook. Timeline entry only. Use for multi-hour work.

### Aborting vs Failing

| When | Command | Effect |
|---|---|---|
| Any recoverable failure (default) | `notify --fail` | `aborted`; report in `payload.artifact.report` for owner to read |
| Truly unrecoverable at startup (broken sandbox, no info to give) | `boid action send --task "$BOID_TASK_ID" --type abort --payload '{"code":"...","message":"..."}'` | `aborted`; no payload |

Prefer `--fail`. Use `action send --type abort` only when nothing can be reported.

### Reopen semantics

When this task is reopened, the new instruction is **appended** as the last element
of `instructions.yaml`. Earlier elements are context only — act only on the tail.
Reopen always spawns a fresh agent process (no harness session is carried over
from the previous turn) — re-read every file in *Step 0* before deciding what
to do, even if you "feel" like you already know the task.

---

## Supervisor Mode

*Triggered when `environment.yaml` `readonly: true`.*

A supervisor **orchestrates**: reads the request, decomposes it into child tasks,
monitors them until terminal, integrates results, and exits. It never edits project
files. Implementation always happens in child executor tasks.

### Dynamic instruction generation (default pattern)

- **Root task**: boots from `project.yaml`'s `default_instruction` (set by the
  project owner). Do not override unless the active instruction or user reply
  explicitly says to.
- **Child tasks**: the supervisor generates each child's instruction dynamically
  and passes it in the `instructions` field of `boid task create`. Tailor the
  instruction to the child's specific role, context, and scope — do not just
  forward the supervisor's own instruction.

This is the default flow: root = fixed boot instruction, descendants = dynamically
generated by their parent.

### Overall flow

1. **Plan** — Read title + active instruction; decide child decomposition and order.
2. **(Conditional) approval ask** — Present the plan via `boid task ask` when the
   request leaves room for interpretation. Skip only when behavior and granularity
   are obvious.
3. **Create → Monitor → Integrate** — For each child: create, poll until terminal,
   run the integration step from the active instruction, then move to the next.
4. **Re-plan** — If a child's result changes the plan, spawn additional children
   or escalate via `boid task ask`.
5. **Exit** — All children terminal → `notify --done` (or `--fail`).

Even with a single child, remain as supervisor and see it through.

### Sequencing children

```bash
A=$(boid task create <<YAML | awk '{print $3}'
title: phase A
behavior: executor
ref: phase-a
instructions:
  - message: |
      <dynamically generated instruction for phase A>
auto_start: true
YAML
)
# monitor A until terminal, run integration step, then:
B=$(boid task create <<YAML | awk '{print $3}'
title: phase B (uses A's result)
behavior: executor
ref: phase-b
instructions:
  - message: |
      <dynamically generated instruction for phase B, incorporating A's findings>
auto_start: true
YAML
)
```

Create children in sequence inside the supervisor session. Parallel creation only
when children are genuinely order-independent.

### Creating child tasks

`boid task create` reads YAML/JSON from stdin and prints
`task created: <id> (<status>)`. `parent_id` is auto-filled from `BOID_TASK_ID`.

Key fields:

- **`ref`** — **required for every child.** Stable role slug unique within this
  parent (e.g. `migrate-schema`, `write-tests`). Never random. Omitting `ref` is
  a hard error in-sandbox.
- `behavior` — `executor` for implementation tasks. Omit to default to `supervisor`
  for sub-orchestration.
- `instructions` — 1-entry array for dynamic instruction generation (see above);
  2+ entries = complete replacement.
- `auto_start: true` — start immediately.
- `base_branch` — worktree fork point. Inherits project-top if omitted.

Full reference: [references/builtins.md](references/builtins.md).

#### Resume: reconcile before create

`boid task create` is idempotent when `ref` is stable: a second call with the same
`(ref, parent_id)` returns the **existing** task. Re-running the entire create loop
on resume is safe — existing tasks are returned as-is.

For efficiency, use `artifact.children` to skip creates of already-terminal tasks:

```bash
children_json=$(boid task show "$BOID_TASK_ID" --field payload.artifact.children 2>/dev/null || echo '{}')
CHILD_A=$(printf '%s' "$children_json" | python3 -c "import json,sys; d=json.load(sys.stdin); print(d.get('phase-a',{}).get('id',''))" 2>/dev/null || echo "")

if [ -z "$CHILD_A" ]; then
  CHILD_A=$(boid task create <<YAML | awk '{print $3}'
title: phase A
behavior: executor
ref: phase-a
instructions:
  - message: |
      <instruction>
auto_start: true
YAML
  )
fi
```

#### Overriding default_instruction fields (use sparingly)

A 1-entry `instructions:` array merges per-field with `default_instruction`; empty
fields inherit. Use only when the active instruction or user reply explicitly
requests an override (e.g. "use opus for the heavy refactor").

```yaml
instructions:
  - model: claude-opus-4-8   # message / agent / type inherit from default_instruction
```

### Monitoring children

> **Critical — do not poll in the foreground.** The harness blocks foreground
> `sleep` and cancels parallel foreground polls. Use a background Monitor tool call.

Arm one **Monitor** per child. The watch script emits on every status change and
exits when the child is terminal:

```bash
# Monitor tool — command:
CHILD="<child-id>"
prev=""
while true; do
  st=$(boid task show "$CHILD" --field status 2>/dev/null || echo "")
  if [ -n "$st" ] && [ "$st" != "$prev" ]; then
    echo "child $CHILD -> $st"
    prev="$st"
  fi
  case "$st" in done|aborted) exit 0 ;; esac
  sleep 30
done
```

Set `timeout_ms` to match expected duration (default 300000; up to 3600000 for
long builds). Use `persistent: true` for very long children. After arming,
**stop generating** — you are notified on each event.

On notification, branch by status:

- `awaiting` — child called `boid task ask` (or, legacy, `notify --ask`).
  Handle it (see Handling Awaiting), then keep waiting. The same Monitor stays
  armed; no re-arm needed unless you yourself escalate via `boid task ask`.
- `done` — child self-reported success. Verify and integrate.
- `aborted` — child failed. Diagnose and decide.

**Re-arm only when you yourself paused** via `boid task ask`. The daemon
SIGTERMs your runtime and the Monitor dies with it; arm a fresh Monitor on
resume.

Full status semantics: [references/state-machine.md](references/state-machine.md).

### Handling Done

```bash
short=$(echo "$child" | cut -c1-8)

# Layer A: child's structured self-report
boid task show "$child" --field payload.artifact.report

# Layer B: independent git check
git log "main..boid/$short"
git diff "main..boid/$short"
gh pr view --head "boid/$short" 2>/dev/null || true

# Layer C: shape diagnostics
last_job=$(boid job list --task "$child" --output json | jq -r '.[0].id')
boid job log "$last_job" | tail -200
```

Then choose:

```bash
# Accept — proceed to next child or integration.

boid task reopen "$child" -m "<what to change>"   # revise
boid task abort  "$child"                          # repudiate (rare)
boid task ask "<escalation question for owner>"   # escalate
```

If `payload.artifact.report` is empty or missing `summary`, treat as
**missing-report anomaly**: reopen with `-m "Re-run with payload.artifact.report
populated (summary, evidence, verification)."`.

### Reporting your own done (daemon validates — do not fabricate)

Two daemon-enforced rules:

1. **Never report done while a child is open.** Wait for the actual Monitor `done`
   event. The daemon rejects `notify --done` while any child is still open.

2. **Never cite a commit/branch you have not seen in real git output.** If your
   done involves a release, record it in `payload.artifact.report.release` from
   the **actual** command output:

   ```bash
   merged=$(git rev-parse HEAD)
   boid task update "$BOID_TASK_ID" --payload-file - <<EOF
   artifact:
     report:
       release:
         commit: "$merged"
         branch: "$BRANCH"
         pushed: true
   EOF
   boid task notify "$BOID_TASK_ID" --done "Released $merged to $BRANCH."
   ```

   The daemon verifies `release.commit` exists in the repo.

### Handling Aborted

```bash
last_action=$(boid task show "$child" --field 'actions[-1].type')
# "fail"  → child self-reported; read action payload + artifact.report
# "abort" → forced; read lifecycle.abort.message
```

Options: `boid task reopen "$child" -m "<hint>"` / create fresh child / escalate.

### Handling Awaiting

```bash
question=$(boid task show "$child" --field awaiting.question)
```

Then:

```bash
boid task answer "$child" "<reply>"                  # answer (works for task ask)
boid task reopen "$child" -m "<redirect>"            # redirect
boid task ask "<question for own owner>"             # escalate
```

### Detecting Stuck Children

Two failure modes:

1. **Silent exit** — `claude` exits without `notify --done`/`--fail` and never
   parked on a `boid task ask`; child stays `executing` with no live job.
2. **PTY hang** — `claude` still running but PTY idle past threshold.

Detect inside the **same Monitor watch script**:

```bash
CHILD="<child-id>"; IDLE_MAX=600
prev=""; stuck=""
while true; do
  st=$(boid task show "$CHILD" --field status 2>/dev/null || echo "")
  if [ -n "$st" ] && [ "$st" != "$prev" ]; then echo "child $CHILD -> $st"; prev="$st"; stuck=""; fi
  if [ "$st" = "executing" ] && [ -z "$stuck" ]; then
    lj=$(boid job list --task "$CHILD" --output json 2>/dev/null | jq -r '.[0].id // empty')
    if [ -n "$lj" ]; then
      ljs=$(boid job show "$lj" --field status 2>/dev/null || echo "")
      idle=$(boid job show "$lj" --field transcript_idle_seconds 2>/dev/null || echo 0)
      if [ "$ljs" != "running" ] || [ "${idle:-0}" -gt "$IDLE_MAX" ]; then
        echo "child $CHILD -> stuck (job=$ljs idle=${idle}s)"; stuck=1
      fi
    fi
  fi
  case "$st" in done|aborted) exit 0 ;; esac
  sleep 30
done
```

`IDLE_MAX` guidance: 600 default, 300 fast-iteration, 1800 long-build.

On `stuck`, confirm with **one** read (not a poll loop) then:
- **Reopen** with a status-check message
- **Abort** if clearly unrecoverable
- **Escalate** via own `boid task ask`

Note: `boid task notify --progress` does **not** update `transcript.log`.

### Lifecycle accountability (supervisor as owner)

You **own the lifecycle of every child task you create**. Children that enter
`awaiting` are asking **you**, not the user — the daemon hardcodes "only root
tasks (`parent_id == \"\"`) fire user-facing notify hooks". For each child
status transition, you choose:

| Child status | Source | Your response options |
|---|---|---|
| `done` | child called `notify --done` | Verify (Layers A–C); accept / `reopen` to revise / `abort` (rare) / escalate |
| `aborted` | child called `notify --fail` or `action send --type abort` | Diagnose; `reopen` with hint / create fresh child / leave aborted / escalate |
| `awaiting` | child called `boid task ask` (mid-flight question) | `task answer` to reply / `reopen` to redirect / escalate up |

In all cases, "escalate up" means your own `boid task ask` (or `notify --done` /
`--fail`) toward your own parent (or the user, for root supervisors).

See [docs/plans/lifecycle-accountability.md](../../../docs/plans/lifecycle-accountability.md) for the full contract.

### When to ask (plan approval)

- **Required**: ambiguous request, multiple reasonable decompositions.
- **Skip**: single child, behavior obvious from request.
- **Mid-flight**: ≥ half children aborted; hard cap reached; unexpected fact forces
  plan revision.

Plan presentation template:

````markdown
## Implementation Plan

### Child Tasks
| # | title | behavior | parallel/serial | estimate |
|---|-------|----------|-----------------|----------|
| 1 | ... | executor | - | a few hours |
| 2 | ... | executor | after 1 | a few hours |

### Risks & Assumptions
- ...

### Decision needed
- A. Proceed with the plan above
- B. Present a revised proposal
- C. Cancel
````

### Hard cap (runaway prevention)

Enforce in your own control flow — the daemon does not:

- **> 20 children** created in this session → `boid task ask`
- **> 12 hours** since planning started → `boid task ask`

---

## Executor Mode

*Triggered when `environment.yaml` `readonly: false`.*

An executor **implements**: edits files in its worktree, runs tests, commits, and
exits. The parent supervisor handles integration — the executor only commits and
reports.

### Workflow

1. **Read** — title + description in `task.yaml`, the active instruction at the
   tail of `instructions.yaml`. Confirm worktree path and writability via
   `environment.yaml`.
2. **Implement** — make the code / test / doc changes. Stay inside the executor's
   worktree.
3. **Verify** — run the project's quick verification (tests + lint) before
   committing. The active instruction usually names the verification command.
4. **Release** — follow the release steps in the active instruction (e.g. `git add`
   + `git commit`, or invoking a project release skill like `/dev-pr-flow`).
5. **Report & Exit** — write `payload.artifact.report`, then `notify --done/fail/ask`.

### Writing the final report

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

`summary` is required. The owner reads this as the **canonical Layer A source**.
If it is empty or missing, the supervisor treats it as a missing-report anomaly
and reopens you.

### Executor rules

- Only edit files inside your worktree (path in `environment.yaml`). Anything
  outside is lost when the worktree is removed on `done`.
- Follow constraints in `environment.yaml` (`network.restricted`, `tools`).
- **Always commit before exiting.** Uncommitted changes vanish with the worktree.
- Do not spawn child tasks. Decomposition belongs to the supervisor.
- Do not write to `instructions` in the task payload — it is delivered as the
  read-only file `instructions.yaml`.

---

## References

- [references/builtins.md](references/builtins.md) — flags and fields for `boid task` / `boid job` / `boid action` (supervisor mode).
- [references/state-machine.md](references/state-machine.md) — child task statuses, transitions, and supervisor reactions.
- [docs/plans/lifecycle-accountability.md](../../../docs/plans/lifecycle-accountability.md) — full lifecycle contract.
- [docs/plans/agent-aware-boid.md](../../../docs/plans/agent-aware-boid.md) — Track B design decisions.
