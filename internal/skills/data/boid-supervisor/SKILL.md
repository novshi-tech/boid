---
name: boid-supervisor
description: Runs a supervisor task (readonly orchestrator) for the boid orchestrator.
  Reads task.yaml + instructions.yaml, creates child executor tasks via
  `boid task create`, monitors them in order, integrates results per the active
  instruction, and consults the user via `boid task notify --ask` when needed.
---

# boid Supervisor

A supervisor task **triages** a request, creates child executor tasks, and **monitors them** until completion. The supervisor is **readonly** — it reads the working tree and runs `git` queries but never edits project files. Implementation always happens in child executor tasks.

## Context to Read First

Read these files from the sandbox before doing anything else (full schema in [boid-sandbox / data-model.md](../boid-sandbox/references/data-model.md)). **Always re-read on every invocation — including resume after a user reply and reopen with a new instruction.** `claude --resume` carries chat history but does **not** guarantee that prior tool-call inputs remain accessible (in particular, the body of your own previous `notify --ask` is frequently missing). If a user reply or reopen instruction looks fragmentary or context-free, the active instruction and payload on disk almost certainly have the missing piece — read them before deciding "I don't have context".

| File | Contents |
|---|---|
| `~/.boid/context/task.yaml` | Title, description, status, behavior |
| `~/.boid/context/instructions.yaml` | Instructions array; **the last element is active** |
| `~/.boid/context/payload.yaml` | Existing artifacts (children, prior asks) |
| `~/.boid/context/environment.yaml` | Sandbox constraints (readonly, network) |

The **active instruction** carries the project-specific integration policy — for example, whether to fast-forward each child branch locally or to merge a PR via `gh`. This skill describes only the generic orchestration loop; integration details come from the instruction. If the instruction is silent on integration, ask the user via `notify --ask` before guessing.

`.boid/project.yaml` declares the available `task_behaviors`. Canonical names: `supervisor` (this skill — readonly) and `executor` (writable; see `/boid-executor`).

## Lifecycle Accountability

You **own the lifecycle of every child task you create**. Children that enter `awaiting` are asking **you**, not the user — the daemon hardcodes "only root tasks (parent_id == \"\") fire user-facing notify hooks". For each child status transition, you choose:

| child status | source | your response options |
|---|---|---|
| `done` | child called `notify --done` | verify and either leave as-is (accept) or `reopen` to revise |
| `aborted` | child called `notify --fail` or `action send --type abort` | inspect the failure, then `reopen` with a hint, leave aborted, or escalate up |
| `awaiting` | child called `notify --ask` (mid-flight question) | `task answer` to reply, `reopen` to redirect, or escalate up |

In all cases, "escalate up" means your own `notify --ask` (or `--done` / `--fail`) toward your own parent (or the user, for root supervisors).

See [docs/plans/lifecycle-accountability.md](../../../docs/plans/lifecycle-accountability.md) for the full contract.

## Overall Flow

1. **Plan** — Read the title and the active instruction; decide on child decomposition and ordering.
2. **(Conditional) approval ask** — When the request leaves room for interpretation, present the plan via `boid task notify --ask` and wait for approval (see "When to ask").
3. **Create → Monitor → Integrate, in order** — For each child: create with `boid task create`, poll until terminal, run the integration step from the active instruction, then move to the next.
4. **Re-plan (if needed)** — If a child's result changes the plan, spawn additional children or escalate via `notify --ask`.
5. **Exit** — Once all children are terminal, exit autonomously or via an exit-confirmation ask.

Even with only one child, remain as supervisor and see it through. Users rely on the supervisor as a single point of visibility ("the parent is watching on my behalf").

## Sequencing Children

Sequence children **inside the supervisor session** by creating → monitoring → creating-next. Keep ordering explicit in the supervisor's control flow; it makes the intent visible from the session transcript.

Use parallel creation only when children are genuinely order-independent.

```bash
A=$(boid task create <<YAML | awk '{print $3}'
title: phase A
behavior: executor
description: ...
auto_start: true
YAML
)
# … monitor A until terminal, run integration step, then …
B=$(boid task create <<YAML | awk '{print $3}'
title: phase B (uses A's result)
behavior: executor
description: ...
auto_start: true
YAML
)
```

## Creating Child Tasks

`boid task create` reads YAML/JSON from stdin and prints `task created: <id> (<status>)`. Required field: `title`. `parent_id` is auto-filled from `BOID_TASK_ID`.

Most commonly used optional fields:

- `behavior` — `executor` for implementation. Omit to default to `supervisor` (re-delegate triage to the child).
- `description` — detailed instructions for the child agent.
- `auto_start: true` — start immediately on create.
- `base_branch` — branch to fork the worktree from. Omit to inherit from project-top.

Full field reference: [references/builtins.md](references/builtins.md).

## Monitoring Children

Poll each child until terminal (`done` / `aborted`):

```bash
while true; do
  case "$(boid task show "$id" --field status)" in
    done|aborted) break ;;
    awaiting)     handle_awaiting "$id" ;;
  esac
  sleep 60
done
```

The status itself carries the intent — there is no longer a textual prefix to parse:

- `done` — child called `notify --done` (success self-report). See [Handling Done](#handling-done).
- `aborted` — child called `notify --fail` or `action send --type abort`. See [Handling Aborted](#handling-aborted).
- `awaiting` — child called `notify --ask` (mid-flight question). See [Handling Awaiting](#handling-awaiting).

Adjust the `sleep` interval to the implementation scale (30s for small tasks, 2–5 min for larger builds).

Full status semantics: [references/state-machine.md](references/state-machine.md). Diagnostic commands: [references/builtins.md](references/builtins.md).

## Handling Done

The child believes it succeeded. Verify before integrating:

```bash
short=$(echo "$child" | cut -c1-8)

# Layer A: child's structured self-report (one-shot canonical source)
boid task show "$child" --field payload.artifact.report

# Layer B: independent fact-check via git / gh (push not required — local branch is enough)
git log "main..boid/$short"
git diff "main..boid/$short"
gh pr view --head "boid/$short" 2>/dev/null || true

# Layer C: shape diagnostics (size / tail / last update — not for content parsing)
last_job=$(boid job list --task "$child" --output json | jq -r '.[0].id')
boid job log "$last_job" | tail -200
```

Then choose one:

```bash
# Accept: leave the child in `done` and proceed to integration / next child.
# (No action needed — `done` is already terminal.)

boid task reopen "$child" -m "<what to change>"                              # revise
boid task abort  "$child"                                                    # repudiate (rare; usually reopen is enough)
boid task notify "$BOID_TASK_ID" --message "..." --ask "<escalation>"        # escalate up
```

If `payload.artifact.report` is empty or missing required fields, treat that as a **missing-report anomaly**: reopen with `-m "Re-run with payload.artifact.report populated (summary, evidence, verification)."` rather than accepting on faith.

## Handling Aborted

The child reported failure (via `notify --fail`) or aborted outright (via `action send --type abort`). Same diagnostic toolbox as Done (Layers A–C). Common outcomes:

- Recoverable with a hint → `boid task reopen <child> -m "..."` (aborted → executing)
- Genuinely unrecoverable → leave the child aborted, redesign or escalate
- You don't know → escalate via your own `notify --ask`

To distinguish self-failure (`notify --fail`) from forced abort, check the last action:

```bash
last_action=$(boid task show "$child" --field 'actions[-1].type')
# last_action == "fail"  → child self-reported; read action payload + artifact.report
# last_action == "abort" → forced; read lifecycle.abort.message and payload.code
```

## Handling Awaiting

The child has a mid-flight question. Read it and decide:

```bash
question=$(boid task show "$child" --field awaiting.question)
```

Then one of:

```bash
boid task answer "$child" "<reply>"                                          # answer in your context
boid task reopen "$child" -m "<redirect>"                                    # redirect via reopen
boid task notify "$BOID_TASK_ID" --message "..." --ask "<question for own parent>"  # escalate up
```

## Detecting Stuck Children

Two distinct failure modes require detection:

1. **Silent exit** — `claude` exits without issuing `notify --ask`, leaving the child in `executing` with no live job (`job.status != running`, `updated_at` old).
2. **PTY hang** — `claude` is still running (`job.status == running`) but the PTY is waiting for input and `transcript.log` has had no new writes for a long time (`transcript_idle_seconds` large).

On each poll iteration, check both:

```bash
status=$(boid task show "$child" --field status)
if [ "$status" = "executing" ]; then
  last_job=$(boid job list --task "$child" --output json | jq -r '.[0].id // empty')
  if [ -n "$last_job" ]; then
    last_status=$(boid job show "$last_job" --field status)
    idle=$(boid job show "$last_job" --field transcript_idle_seconds 2>/dev/null)
    idle=${idle:-0}
    last_updated=$(boid job show "$last_job" --field updated_at)
    # silent exit: job finished but task didn't transition
    if [ "$last_status" != "running" ] && [ <updated_at old check> ]; then
      handle_stuck "$child" "job exited without state transition"
    # PTY hang: job still running but transcript has been idle too long
    elif [ "$last_status" = "running" ] && [ "$idle" -gt 600 ]; then
      handle_stuck "$child" "PTY idle ${idle}s"
    fi
  fi
fi
```

Threshold guidance for `transcript_idle_seconds`:
- Default: **600** (10 min) — covers most executor tasks
- Fast iteration: **300** (5 min) — when the executor should be actively writing
- Long build / slow network: **1800** (30 min) — when legitimate pauses are expected

Note: `boid task notify --progress` does **not** update `transcript.log` (it goes through the broker, not the PTY). Only actual agent output (e.g. tool results, text written to the PTY) advances the mtime.

Decisions:

- **Reopen with a status check** — `boid task reopen "$child" -m "Status check: where are you? Emit notify --ask 'done_request: ...' if you finished, or describe what you need."`
- **Abort** when clearly unrecoverable
- **Escalate up** when you cannot decide

Stuck-child detection is the structural safety net for "agent forgot to call `notify --ask`". Treat it as routine — not exceptional — until executors universally emit explicit done_request.

## Q&A Pattern (asking the user)

To pause and ask the user, call:

```bash
boid task notify "$BOID_TASK_ID" \
  --message "<short summary for the push notification>" \
  --ask "<full question body>"
```

Both `--message` (short) and `--ask` (full body) are required. The call transitions the task to `awaiting`, fires the notify hook, and the daemon then signals your runtime (SIGUSR1 → `run-agent.py` SIGTERMs `claude`; bash and the EXIT trap survive) — **just stop generating after the call returns**. No sentinel output, no explicit exit; the EXIT trap fires `boid job done --output-file payload_patch.json` as the canonical completion call, preserving your session id for the next resume.

When the user replies, the kit re-invokes you with:

| Env | Meaning |
|---|---|
| `BOID_USER_ANSWER` | The user's reply text |
| `BOID_QUESTION_ID` | The turn ID |

Canonical first-or-resume branching:

```bash
if [ -n "$BOID_USER_ANSWER" ]; then
  case "$BOID_USER_ANSWER" in
    A|*approve*|*proceed*) ... ;;
    B|*revise*) ... ;;
    *) ... ;;   # rejection / cancel
  esac
else
  # First invocation. Decide whether to ask or proceed.
  ...
fi
```

Multiple Q&A turns are fine — each `--ask` replaces the previous pending answer.

**Never use `notify` without `--ask` for decision branches.** Bare `notify` is FYI-only and does not block.

### Reopen with a Question / Explanation Request

When the active instruction is a question about prior behavior ("explain why you stopped", "summarize what happened", "what is the cause"), the answer **still goes through `boid task notify`** — never as bare assistant text. The Claude session has no other channel to your owner; whatever you write as a closing paragraph in the agent transcript is invisible.

- If the answer naturally invites a next-step decision ("should I proceed with X?"), put the explanation in `--ask` and present the follow-up options there.
- If the answer is purely informational, wrap it as `--done "<explanation>"` — the task transitions to `done` with the message on the timeline. Your owner reads it and reopens if anything is off.

A reopen turn that ends with bare assistant text leaves the task stuck in `executing` with no visible response surfaced. This is the failure mode behind the 2026-05-14 incident where a correct diagnostic reply never reached the user — generalized into the lifecycle-accountability model.

## When to Ask (notify --ask)

- **Plan-approval (conditionally required)** — Before creating children, present the plan via `notify --ask` and obtain approval. Skip when:
  - The request is specific enough that there is little room for interpretation
  - There is only one child and behavior/granularity are obvious from the request
- **Mid-flight**:
  - Half or more children aborted
  - Hard cap reached (20 children / 12 hours)
  - Unexpected fact from a child's artifact forces a plan revision

When in doubt, ask.

### Plan presentation template

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

Always include all three blocks.

## Hard Cap (runaway prevention)

The daemon does not enforce caps; enforce them in your own control flow.

- **>20 children** created in this session → stop and `notify --ask`
- **>12 hours** since planning started → stop and `notify --ask`

These numbers can be overridden by the active instruction.

## Exit Handling (required)

**Every invocation** — first start, user-reply resume, reopen — must terminate in a `boid` command. boid records `notify` (`--done` / `--fail` / `--ask`) and EXIT-trap `job done` actions; it does **not** record agent transcript text. Ending the session with bare assistant text leaves the task stuck in `executing` with no visible response to your owner.

The contract is identical for child (`parent_id != ""`) and root (`parent_id == ""`) supervisors — emit exactly one of:

- **Subtree complete** — your children are all terminal, the request is satisfied. Transitions to `done`:
  ```bash
  boid task notify "$BOID_TASK_ID" \
    --message "<short summary>" \
    --done "<what your subtree achieved>"
  ```
- **Subtree failed** — you could not complete the request. Transitions to `aborted`:
  ```bash
  boid task notify "$BOID_TASK_ID" \
    --message "<short summary>" \
    --fail "<what went wrong, what you tried>"
  ```
- **Need a decision before continuing** — mid-flight question. Transitions to `awaiting`:
  ```bash
  boid task notify "$BOID_TASK_ID" \
    --message "<short summary>" \
    --ask "<question>"
  ```

After the call returns, the daemon SIGTERMs your runtime — **just stop generating**. For root supervisors the user receives a desktop notification (with deep-link to the task page); for child supervisors the parent's polling picks up the new status. For root supervisors specifically, `--done` is the canonical close — the user reopens or escalates if anything is off, which is cheaper than an extra awaiting + confirm round-trip for content the user typically does not read carefully.

> Do **not** call `boid agent stop` or `boid job done` directly. The first is the legacy silent-termination path that motivated this model; the second unregisters the broker token before the EXIT trap runs and silently drops your `payload_patch.json`.

> No exit safety net: the claude-code kit no longer auto-fires `boid agent stop` when your response loop ends (the Stop hook was removed in lifecycle-accountability Phase 2.a). Ending a turn with bare assistant text leaves the task stuck in `executing` — **always exit via an explicit `notify --done` / `--fail` / `--ask`** before ending the turn.

## References

- [references/builtins.md](references/builtins.md) — flags and fields for the `boid task` / `boid job` / `boid action` subcommands available inside the sandbox.
- [references/state-machine.md](references/state-machine.md) — child task statuses, manual / event-driven / auto transitions, and how the supervisor reacts to each.
- [docs/plans/lifecycle-accountability.md](../../../docs/plans/lifecycle-accountability.md) — design contract: how children's lifecycle events route to the supervisor, and the parent-id-based notify gate.
