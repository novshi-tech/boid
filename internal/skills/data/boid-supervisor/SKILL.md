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

You **own the lifecycle of every child task you create**. Children that enter `awaiting` are asking **you**, not the user — the daemon hardcodes "only root tasks (parent_id == \"\") fire user-facing notify hooks". For each child event, you choose:

- **answer** (`boid task answer`) — child asked a question
- **confirm done** (`boid action send --type done`) — child reports completion and you agree
- **reopen** (`boid task reopen <id> -m "..."`) — push back with instructions
- **abort** (`boid task abort <id>`) — unrecoverable, stop the child
- **escalate up** (your own `notify --ask`) — you cannot decide; ask your own parent (or user)

See [docs/plans/lifecycle-accountability.md](../../../docs/plans/lifecycle-accountability.md) for the full contract.

## Overall Flow

1. **Plan** — Read the title and the active instruction; decide on child decomposition and ordering.
2. **(Conditional) approval ask** — When the request leaves room for interpretation, present the plan via `boid task notify --ask` and wait for approval (see "When to ask").
3. **Create → Monitor → Integrate, in order** — For each child: create with `boid task create`, poll until terminal, run the integration step from the active instruction, then move to the next.
4. **Re-plan (if needed)** — If a child's result changes the plan, spawn additional children or escalate via `notify --ask`.
5. **Exit** — Once all children are terminal, exit autonomously or via an exit-confirmation ask.

Even with only one child, remain as supervisor and see it through. Users rely on the supervisor as a single point of visibility ("the parent is watching on my behalf").

## Sequencing Children

Sequence children **inside the supervisor session** by creating → monitoring → creating-next. Do **not** use boid's `depends_on` / `depends_on_payload` features — keeping ordering explicit in the supervisor's control flow makes the intent visible from the session transcript and avoids a parallel ordering mechanism.

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
  esac
  sleep 60
done
```

- On `done` — read its artifact (`boid task show <id> --field artifact.<key>`), run the integration step from the active instruction, then decide on the next child.
- On `aborted` — read `lifecycle.abort.message`, diagnose with `boid job list/show/log` (note: `job log` returns the runtime's **raw PTY capture** — ANSI-laden, useful for shape diagnostics rather than structured parsing), then retry / redesign / escalate.
- On `awaiting` — the child is asking **you**. See [Handling Child Awaiting](#handling-child-awaiting).

Adjust the `sleep` interval to the implementation scale (30s for small tasks, 2–5 min for larger builds).

Full status semantics: [references/state-machine.md](references/state-machine.md). Diagnostic commands: [references/builtins.md](references/builtins.md).

## Handling Child Awaiting

When a child enters `awaiting`, the daemon has already gated user-facing notifications (children never reach the user directly). Read the question and parse the **prefix** to identify intent:

| prefix | meaning |
|---|---|
| `done_request:` | child believes it has completed; verify and confirm or reopen |
| `failure_report:` | child reports an unrecoverable problem; decide reopen / abort / escalate |
| (no prefix) | generic question / decision request; answer or escalate |

```bash
question=$(boid task show "$child" --field awaiting.question)
```

### done_request

Verify before confirming. Pull from the three diagnostic layers:

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
boid action send --task "$child" --type done                       # confirm
boid task reopen "$child" -m "<what to change>"                    # reopen with revision
boid task abort "$child"                                           # unrecoverable
boid task notify "$BOID_TASK_ID" --message "..." --ask "done_request: ..."  # escalate up
```

If `payload.artifact.report` is empty or missing required fields, treat that as a **missing-report anomaly**: reopen with `-m "Re-run with payload.artifact.report populated (summary, evidence, verification)."` rather than confirming on faith.

### failure_report

Read the failure detail. Same toolbox as `done_request` (Layers A–C) — just driven by a different intent. Common outcomes:

- Recoverable with a hint → `boid task reopen <child> -m "..."`
- Genuinely unrecoverable → `boid task abort <child>`
- You don't know → escalate via your own `notify --ask`

### bare ask

Answer in your own context, or escalate:

```bash
boid task answer "$child" "<reply>"
# or
boid task notify "$BOID_TASK_ID" --message "..." --ask "<question for your own parent>"
```

## Detecting Stuck Children

Some children fail silently — `claude` exits without issuing `notify --ask`, leaving the child in `executing` with no live job. On each poll iteration, check for staleness:

```bash
status=$(boid task show "$child" --field status)
if [ "$status" = "executing" ]; then
  last_job_id=$(boid job list --task "$child" --output json | jq -r '.[0].id // empty')
  if [ -n "$last_job_id" ]; then
    last_status=$(boid job show "$last_job_id" --output json | jq -r '.status')
    last_update=$(boid job show "$last_job_id" --output json | jq -r '.updated_at')
    # If status != running and updated_at older than your threshold (e.g. 10 minutes),
    # the child is stuck — no live job, but task didn't transition.
  fi
fi
```

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
- If the answer is purely informational and you have a parent (`parent_id != ""`), wrap it as `--ask "done_request: <explanation>"` — your parent supervisor receives it and decides. Bare `--message` (FYI) is timeline-only and your parent never reacts to it.
- If the answer is purely informational and you are a root supervisor, put it in `--message` (FYI mode, no `--ask`) and then exit via `boid agent stop`. Without `--ask`, the task will not transition to `awaiting`, so the user has no built-in reply turn — make sure that is the intent.

A reopen turn that ends with bare assistant text is treated by boid as "no ask, no exit action" → `auto_advance` closes the task to `done` with **no visible response surfaced**. This is the failure mode behind the 2026-05-14 incident where a correct diagnostic reply never reached the user — generalized into the lifecycle-accountability model.

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

**Every invocation** — first start, user-reply resume, reopen — must terminate in a `boid` command. boid records `notify` / `notify --ask` / `agent stop` (and the bash EXIT trap's follow-up `job done`) actions; it does **not** record agent transcript text. Ending the session with bare assistant text is equivalent to leaving it open without action: your owner sees an empty `done` task with no visible response.

You are an agent task yourself — the lifecycle accountability rules apply to your own termination too. Your **owner** is the parent supervisor (`parent_id != ""`) or the user (`parent_id == ""`). Check first:

```bash
parent=$(boid task show "$BOID_TASK_ID" --field parent_id)
```

### Child supervisor (`parent_id != ""`)

Your owner is another supervisor. The daemon will not surface your exit to the user — your parent must see it via `notify --ask`. **Do not call `boid agent stop`** when you have a parent: it would silently transition to `done` with no signal upward (the same anti-pattern that motivated the lifecycle-accountability model).

Emit exactly one of:

- **Subtree complete** — your children are all terminal, the parent's request is satisfied:
  ```bash
  boid task notify "$BOID_TASK_ID" \
    --message "<short summary>" \
    --ask "done_request: <what your subtree achieved>"
  ```
- **Subtree failed** — you could not complete the request:
  ```bash
  boid task notify "$BOID_TASK_ID" \
    --message "<short summary>" \
    --ask "failure_report: <what went wrong, what you tried>"
  ```
- **Need a parent decision before continuing** — `notify --ask "<question>"` (no prefix). Resume picks up after the parent answers.

After `notify --ask` returns, the daemon SIGTERMs your runtime — **just stop generating**. The bash EXIT trap fires `boid job done` to seal your session id.

### Root supervisor (`parent_id == ""`)

Your owner is the user. The daemon fires user-facing notify hooks for your `notify --ask`. Choose **A** or **B** explicitly:

- **A. Autonomous exit** — `boid agent stop "$BOID_JOB_ID"` (the daemon SIGUSR1s your runtime; bash EXIT trap fires `boid job done --output-file payload_patch.json`). Use **only** when **all** of:
  1. All children are terminal with no remaining supervisor work
  2. The user's most recent reply was a closing response ("ok", "thanks") — not a new request
  3. No unanswered asks
  4. The last summary was a completion report with little room for follow-up
- **B. Exit-confirmation ask** — `notify --ask "done_request: <summary>"` (or `failure_report: <error>`). Use this when there is anything worth surfacing — completion confirmation, summary, or pending decision.

When in doubt, choose B.

> Safety net: the claude-code kit registers a `Stop` hook that calls `boid agent stop` whenever your response loop ends. This rescues a forgotten exit — but ends the task silently with no follow-up surfaced to your owner. **Always pick the appropriate option explicitly.** The Stop hook is scheduled for removal in lifecycle-accountability Phase 2; do not rely on it.

## References

- [references/builtins.md](references/builtins.md) — flags and fields for the `boid task` / `boid job` / `boid action` subcommands available inside the sandbox.
- [references/state-machine.md](references/state-machine.md) — child task statuses, manual / event-driven / auto transitions, and how the supervisor reacts to each.
- [docs/plans/lifecycle-accountability.md](../../../docs/plans/lifecycle-accountability.md) — design contract: how children's lifecycle events route to the supervisor, and the parent-id-based notify gate.
