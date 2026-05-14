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

Read these files from the sandbox before doing anything else (full schema in [boid-sandbox / data-model.md](../boid-sandbox/references/data-model.md)):

| File | Contents |
|---|---|
| `~/.boid/context/task.yaml` | Title, description, status, behavior |
| `~/.boid/context/instructions.yaml` | Instructions array; **the last element is active** |
| `~/.boid/context/payload.yaml` | Existing artifacts (children, prior asks) |
| `~/.boid/context/environment.yaml` | Sandbox constraints (readonly, network) |

The **active instruction** carries the project-specific integration policy — for example, whether to fast-forward each child branch locally or to merge a PR via `gh`. This skill describes only the generic orchestration loop; integration details come from the instruction. If the instruction is silent on integration, ask the user via `notify --ask` before guessing.

`.boid/project.yaml` declares the available `task_behaviors`. Canonical names: `supervisor` (this skill — readonly) and `executor` (writable; see `/boid-executor`).

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
  case "$(boid task get "$id" --field status)" in
    done|aborted) break ;;
  esac
  sleep 60
done
```

- On `done` — read its artifact (`boid task get <id> --field artifact.<key>`), run the integration step from the active instruction, then decide on the next child.
- On `aborted` — read `lifecycle.abort.message`, diagnose with `boid job list/show/log`, then retry / redesign / escalate.
- On `awaiting` — the child is asking the user. Keep polling; it returns to `executing` once the user replies.

Adjust the `sleep` interval to the implementation scale (30s for small tasks, 2–5 min for larger builds).

Full status semantics: [references/state-machine.md](references/state-machine.md). Diagnostic commands: [references/builtins.md](references/builtins.md).

## Q&A Pattern (asking the user)

To pause and ask the user, call:

```bash
boid task notify "$BOID_TASK_ID" \
  --message "<short summary for the push notification>" \
  --ask "<full question body>"
```

Both `--message` (short) and `--ask` (full body) are required. The call transitions the task to `awaiting`, fires the notify hook, and ends your session naturally — **do not wait in the session**, just stop generating.

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

When all children are terminal, the supervisor **must execute exactly one of the following**. Leaving the session open without action is forbidden (users cannot tell the supervisor finished unless a hook fires).

- **A. Autonomous exit** — `boid job done "$BOID_JOB_ID" --exit-code 0`
- **B. Exit-confirmation ask** — Call `notify --ask` to confirm closing. On resume, execute A if the user approves; otherwise continue with the requested work.

Choose A only when **all** of:

1. All children are terminal with no remaining supervisor work
2. The user's most recent reply was a closing response ("ok", "thanks") — not a new request
3. No unanswered asks
4. The last summary was a completion report with little room for follow-up

When in doubt, choose B.

## References

- [references/builtins.md](references/builtins.md) — flags and fields for the `boid task` / `boid job` / `boid action` subcommands available inside the sandbox.
- [references/state-machine.md](references/state-machine.md) — child task statuses, manual / event-driven / auto transitions, and how the supervisor reacts to each.
