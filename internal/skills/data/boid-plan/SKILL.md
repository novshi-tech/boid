---
name: boid-plan
description: Runs a plan task (readonly supervisor) for the boid orchestrator.
  Reads task.yaml title/description, selects the appropriate behavior from
  .boid/project.yaml task_behaviors, creates child tasks via the `boid task create`
  builtin, and monitors them as a supervisor until completion. Spawns additional
  tasks based on child state and notifies the user when needed.
---

> **Deprecated.** This skill is the legacy entry point for the `plan` behavior.
> New projects should use [`/boid-supervisor`](../boid-supervisor/SKILL.md)
> directly. The content below is kept for back-compat during the alias period
> (Phase 5 will remove this skill). The runtime alias `plan → supervisor` keeps
> existing `project.yaml` files working without changes.

# boid Plan

A plan task **triages** a request, creates child tasks with the appropriate behavior, and **monitors them as a supervisor** until completion. The plan itself is readonly - it can read `git` but does not edit files.

## Overall Flow

1. **Plan**: Read the task and decide on behavior and decomposition granularity.
2. **(Conditional) approval ask**: When the request leaves room for interpretation, present the plan and obtain approval via `boid task notify --ask` (see "When to ask"). Skip when the criteria allow.
3. **Create**: Use `boid task create` to create one or more child tasks. Note the task IDs returned.
4. **Monitor**: Periodically check child status and wait.
5. **Re-plan (if needed)**: Based on child results, create additional or revised tasks. Consult the user via `boid task notify --ask` if stuck.
6. **Exit**: Once all children reach `done` / `aborted`, exit autonomously or via an exit-confirmation ask (see "Exit Handling").

Even with only one child task, remain as supervisor and see it through to completion. Simply watching the state has value for users ("the parent is watching on my behalf"), meaning they don't need to check individual child sessions directly.

## Q&A Pattern (asking the user)

To ask the user something, call `boid task notify --ask "<question>"`. This:

- transitions the task to **awaiting** and stores the question in the task payload
- fires the configured notify hook (Web UI alert / push) so the user is alerted
- causes your session to end naturally (your turn finishes; nothing more to do)

When the user replies (via Web UI or `boid task answer`), your session is **automatically resumed** by the kit and these env vars are set on the next invocation:

| Env | Meaning |
|---|---|
| `BOID_USER_ANSWER`      | The user's reply text |
| `BOID_QUESTION_ID`      | The turn ID (matches the one you supplied or that was auto-generated) |
| `BOID_AGENT_SESSION_ID` | Your prior session id (the claude-code kit resumes the conversation transparently from payload-tracked sessions; this env var is informational) |

So the canonical first-or-resume branching looks like:

```bash
if [ -n "$BOID_USER_ANSWER" ]; then
  # Resumed after a Q&A. Branch on the answer.
  case "$BOID_USER_ANSWER" in
    A|*approve*|*proceed*) ... ;;       # accept and continue
    B|*revise*) ... ;;                  # revise the plan and ask again
    *) ... ;;                           # rejection / cancel → write artifact and exit
  esac
else
  # First invocation. Decide whether to ask or proceed.
  ...
fi
```

After calling `notify --ask`, do not "wait in the session" - just stop generating. Multiple Q&A turns are fine; each `--ask` clears the previous `pending_answer` and stores a fresh question.

## Behavior Catalog

Available behaviors are defined in the **`task_behaviors` section of `.boid/project.yaml`**. Read it directly from the sandbox, examine each behavior's `default_instruction.message` (what it does) and settings like `readonly` / `worktree` / `model`, and choose the one that fits the task. SKILL.md does not carry project-specific behavior names.

Omitting `behavior` in `boid task create` routes to **`plan` by default**. Use this when you want to re-delegate as a supervisor (i.e., have the child do its own triage + monitoring).

## Creating Child Tasks

`boid task create` accepts YAML / JSON from stdin. Dependency resolution within the same batch using refs is handled server-side.

```bash
boid task create <<YAML
title: Task title
behavior: <key from task_behaviors in project.yaml, or omit>
description: |
  Implementation instructions for this subtask. Describe what to build and how in detail.
auto_start: true
YAML
```

stdout returns `task created: <id> (<status>)`, which you can capture in a shell variable for monitoring:

```bash
CHILD_A=$(boid task create <<YAML | awk '{print $3}'
title: ...
auto_start: true
YAML
)
```

### Required fields

- `title`: required.
- `parent_id`: optional. When omitted, automatically defaults to the current task ID
  (the `BOID_TASK_ID` env var the sandbox provides). This keeps the new task under the
  supervisor's monitoring scope. Specify it explicitly only when you need to attach the
  new task to a different parent.

### Common fields

| Field | Description |
|---|---|
| `description` | Instructions for the agent. Describe what to implement and how in detail |
| `ref` | Name for dependency resolution (within the same batch) |
| `depends_on` | Array of dependency ref names |
| `depends_on_payload` | Wait condition (see below) |
| `auto_start` | bool. When true, auto-starts once dependencies are resolved |
| `base_branch` | Branch to fork the worktree from. Inherits from behavior if omitted |
| `project_id` | Project to create the task in. Defaults to same as parent |
| `behavior_spec` | Inline behavior definition (for kits that bring their own behavior). Usually not needed if a behavior name defined in project.yaml is used |

Settings like model / readonly are governed by the behavior template (`task_behaviors` in project.yaml). User consultation always goes through `boid task notify --ask` regardless of how the agent is launched, so there is no need to split "interactive plan" and "autonomous plan" into separate behaviors.

## Monitoring as Supervisor

After creating child tasks, wait and watch their status until they complete. The supervisor keeps track of child IDs and polls their status periodically.

```bash
# Check an individual child's status
boid task get ${CHILD_A} --field status
```

Basic monitoring loop:

```bash
CHILDREN="$CHILD_A $CHILD_B $CHILD_C"

while true; do
  PENDING=0
  for id in $CHILDREN; do
    case "$(boid task get "$id" --field status)" in
      done|aborted) ;;
      *) PENDING=$((PENDING + 1)) ;;
    esac
  done
  [ $PENDING -eq 0 ] && break
  sleep 60
done
```

On each iteration:

- Read the artifact of newly `done` children (`boid task get <id> --field artifact.<key>` etc.) and decide whether to create follow-up children
- For `aborted` children, check the cause (`boid task get <id> --field lifecycle.abort.message`), then retry / take a different approach, or escalate to the user
- When stuck on a decision, consult the user via `boid task notify` (see below)

More detailed diagnostics for an aborted child:

```bash
# List all jobs the child ran (ID, handler, status, exit code)
boid job list --task <id>

# Show detail for a specific job (role, exit code, timestamps)
boid job show <job-id>

# Read the runtime transcript when the runtime has not yet been GC'd
boid job log <job-id>
```

Use `boid job list` first to identify which job failed, then `boid job show` for exit code and status, and `boid job log` for the full Claude Code transcript. If `boid job log` prints "log not available", the runtime was already cleaned up (GC runs every 24h); rely on `boid job show` output and `lifecycle.abort.message` in that case.

Adjust the `sleep` interval to the implementation scale (30s for small implementations, 2-5min for large builds/tests).

### Using Claude Code Monitor

You can background the monitoring loop and have Claude Code Monitor read a script that outputs one line per status change. Compare against the previous value to suppress duplicates:

```bash
(prev=""
while true; do
  cur=""
  for id in $CHILDREN; do
    cur="$cur $(boid task get "$id" --field status)"
  done
  if [ "$cur" != "$prev" ]; then
    echo "$cur"
    prev="$cur"
  fi
  sleep 60
done) &
```

Useful for long-running tasks or many children, but a simple foreground loop is sufficient for straightforward cases.

## When to ask (notify --ask)

- **Plan-approval ask (conditionally required)** - Before creating one or more child tasks, present the full plan (list of children / behavior / order / estimated risks) via `boid task notify --ask` and obtain approval. You may skip the approval ask if *any of the following* applies:
  - The user's request is already specific enough that there is little room for interpretation (e.g., "fix this line in this file like this", "rename `xxx` to yyy" - cases where the child task title/description is just a transcription of the request)
  - There is only one child task and the behavior and granularity are obvious from the request

  When in doubt, err on the side of asking.

- When half or more of the children are aborted, or when the plan strategy needs to change
- When the hard cap is reached (20 children / 12 hours)
- When an unexpected fact emerges from a child's artifact and the remaining plan needs to be revised

If something genuinely cannot be answered without the user and you cannot wait (e.g., environment is broken so you cannot continue at all), write the situation to the task artifact and exit. The user will notice via the task list and reopen with new instructions. Prefer `--ask` when waiting is acceptable.

`boid task notify` *without* `--ask` sends a one-way notification (no state transition, no waiting). Use it only for "FYI" milestone signals - decision branches must use `--ask`.

### Plan presentation template

Use the following format when presenting a plan alongside a plan-approval notify:

````markdown
## Implementation Plan

### Child Tasks
| # | title | behavior | parallel/serial | estimate |
|---|-------|----------|-----------------|----------|
| 1 | ... | dev | - | a few hours |
| 2 | ... | dev | after 1 | a few hours |

### Risks & Assumptions
- ...

### Decision needed
- A. Proceed with the plan above
- B. Present a revised proposal
- C. Cancel
````

Always include all three blocks: "Child Tasks table", "Risks & Assumptions", and "Decision options (A/B/C)".

## boid task notify reference

`boid task notify` has two modes:

**Ask mode (`--ask`)**: blocking question. Transitions task `executing → awaiting`, fires the configured notify hook (Web UI alert / push notification), and saves the question in the task payload. Your session ends after the call; you are re-invoked with `BOID_USER_ANSWER` set when the user replies. This is the primary mode for the plan agent.

```bash
boid task notify "$BOID_TASK_ID" \
  --message "Plan ready - approve to dispatch children" \
  --ask "<plan presentation>" \
  --question-id "plan-approve-1"   # optional; auto-generated when omitted
```

`--message` is the short text shown in the notification (push / SMS / email line). `--ask` is the full question body persisted with the task and shown on the Web UI Q&A panel - put options and decision material here.

**FYI mode (no `--ask`)**: one-way notification. No state transition. Use only for milestone signals or final summaries; **never for decision branches**.

```bash
boid task notify "$BOID_TASK_ID" --message "All children dispatched, monitoring"
```

The notification script (configured under `notify.command` in `~/.config/boid/config.yaml`) receives `BOID_TASK_ID` / `BOID_PROJECT_ID` / `BOID_MESSAGE` / `BOID_TASK_URL` (clickable link when `web.public_url` is set) as environment variables.

### Notification semantics

- Each call triggers one notification (calls = notifications)
- Do not use FYI mode for progress reports (child status is visible in task list / Web UI)
- Use ask mode for **decision branches** (which approach to take) and **pre-approval** (before running the full plan)
- Call ask mode once when you reach a point where you cannot proceed without the user

## Exit Handling (required)

When all children have reached `done` / `aborted`, the plan agent **must execute exactly one of the following**. Do not let the session hang without action (users cannot notice the plan has completed unless something fires):

- **A. Autonomous exit**: Execute `boid job done "$BOID_JOB_ID" --exit-code 0`
- **B. Exit-confirmation ask**: Use `boid task notify --ask` to ask the user whether to close the session. On resume, execute A when the user confirms; otherwise proceed with the requested additional work.

Choose A when **all** of the following are met; otherwise B:

1. All children are `done` / `aborted` with no remaining supervisor work
2. The user's most recent reply (from a prior `notify --ask` in this task) was a closing response such as "ok", "got it", "thanks", and not a new request or question
3. There are no unanswered asks (the user has not yet replied)
4. Your last summary was a completion report with little room for the user to ask follow-up questions

When in doubt, choose B. Leaving the session open without doing anything is forbidden - always choose A or B.

#### Example of B

```bash
boid task notify "$BOID_TASK_ID" \
  --message "All child tasks complete - confirm close" \
  --ask "All child tasks complete (child A: done, child B: done). Choose:
A. Mark complete and close the session
B. Request additional work" \
  --question-id "exit-confirm"
# Session ends here. On resume, BOID_USER_ANSWER will be set.
```

On resume, branch on `$BOID_USER_ANSWER`: when it indicates approval ("A" / "ok" / "approve"), run `boid job done "$BOID_JOB_ID" --exit-code 0`. Otherwise, proceed with the requested additional work.

### Relationship with EXIT trap

Calling `boid job done` causes the daemon to send SIGTERM to the process.
When bash exits due to SIGTERM, the EXIT trap fires `boid job done` again,
but the daemon absorbs the double-fire so no double-processing occurs.

## Hard Cap (runaway prevention)

To prevent the supervisor from creating children indefinitely, enforce limits on your own:

- When the cumulative count of child tasks created (in this session) exceeds **20**, stop creating new ones and consult the user via `boid task notify`
- Similarly when **12 hours** have passed since planning started with no sign of completion

Numbers can be adjusted based on implementation scale. The one rule that must be absolute: **never create a supervisor without a cap**.

## Dependencies

For tasks with ordering dependencies, set them on the downstream task:

```bash
boid task create <<YAML
title: Downstream task
behavior: <name>
ref: task-b
description: ...
depends_on:
  - task-a
depends_on_payload: artifact.auto-merge.merged
auto_start: true
YAML
```

Tasks without ordering dependencies should have no `depends_on` (they run in parallel).

Primary `depends_on_payload` values:

| Value | Wait condition |
|---|---|
| `artifact.auto-merge.merged` | Until the dependency task's PR is merged by auto-merge |
| `artifact.children.all_done` | Until all children of the dependency task are done |

If the supervisor manages ordering itself, it can create the next child inside the monitoring loop without using `depends_on_payload`. Using both simultaneously causes conflicting behavior - pick one approach and stick to it.

## Child Phase Splitting

For projects with long dependency chains, the supervisor can manage phases by creating children → waiting for completion → creating the next phase:

```bash
# Phase 1
PHASE1_A=$(boid task create <<<... | awk '{print $3}')
PHASE1_B=$(boid task create <<<... | awk '{print $3}')
# Wait in monitoring loop for PHASE1_A, PHASE1_B to be done

# Plan Phase 2 based on Phase 1 results
PHASE2_A=$(boid task create <<<... | awk '{print $3}')
# ...
```

When the plan is fixed upfront and the supervisor doesn't need to relay, you can also use a declarative pattern with a phase plan as a child:

```bash
boid task create <<YAML
title: Phase 2 Plan
ref: phase2
depends_on: [phase1-a, phase1-b]
depends_on_payload: artifact.children.all_done
auto_start: true
YAML
```

## base_branch

The branch to fork the worktree from (and the merge target of the PR). Inherits from the behavior's `base_branch` if omitted. Specify explicitly when you want derivative tasks to branch off from the current branch during plan execution:

```bash
CURRENT_BRANCH="$(git rev-parse --abbrev-ref HEAD)"

boid task create <<YAML
title: Implementation on feature branch
behavior: <name>
base_branch: ${CURRENT_BRANCH}
description: ...
auto_start: true
YAML
```

If a normal `main`-based branch is sufficient, omit this.

## project_id

Specify when running a task in a different project. Defaults to the same project as the parent. Use project names registered in the environment (e.g., `project_id: boid-kits` when updating `boid-kits` in conjunction with the `boid` main repo).

## Querying Existing Tasks

Before laying out a large plan, you can check existing tasks:

```bash
boid task list --status pending
boid task list --workspace <ws-id>
```

Tasks outside the workspace scope are blocked by the broker (only tasks in your own project / same workspace are listed).
