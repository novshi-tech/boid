---
name: boid-plan
description: Runs a plan task (readonly supervisor) for the boid orchestrator.
  Reads task.yaml title/description, selects the appropriate behavior from
  .boid/project.yaml task_behaviors, creates child tasks via the `boid task create`
  builtin, and monitors them as a supervisor until completion. Spawns additional
  tasks based on child state and notifies the user when needed.
---

# boid Plan

A plan task **triages** a request, creates child tasks with the appropriate behavior, and **monitors them as a supervisor** until completion. The plan itself is readonly - it can read `git` but does not edit files.

## Overall Flow

### Autonomous mode (`BOID_INTERACTIVE` unset / 0)

1. **Plan**: Read the task and decide on behavior and decomposition granularity.
2. **Create**: Use `boid task create` to create one or more child tasks. Note the task IDs returned.
3. **Monitor**: Periodically check child status and wait.
4. **Re-plan (if needed)**: Based on child results, create additional or revised tasks.
5. **Exit**: Once all children reach `done` / `aborted`, exit. If stuck, write the situation to the artifact and exit.

### Interactive mode (`BOID_INTERACTIVE=1`)

1. **Plan**: Read the task and decide on behavior and decomposition granularity.
2. **(Conditional) user-approval notify**: Before creating child tasks, present the plan and obtain approval according to the criteria in the "When to call notify" section.
3. **Create**: Use `boid task create` to create one or more child tasks. Note the task IDs returned.
4. **Monitor**: Periodically check child status and wait.
5. **Re-plan (if needed)**: Based on child results, create additional or revised tasks. Consult the user via notify if needed.
6. **Exit**: Once all children reach `done` / `aborted`, exit.

Even with only one child task, remain as supervisor and see it through to completion. Simply watching the state has value for users ("the parent is watching on my behalf"), meaning they don't need to check individual child sessions directly.

## Behavior Catalog

Available behaviors are defined in the **`task_behaviors` section of `.boid/project.yaml`**. Read it directly from the sandbox, examine each behavior's `default_instruction.message` (what it does) and settings like `readonly` / `worktree` / `model`, and choose the one that fits the task. SKILL.md does not carry project-specific behavior names.

Omitting `behavior` in `boid task create` routes to **`plan` by default**. Use this when you want to re-delegate as a supervisor (i.e., have the child do its own triage + monitoring).

## Creating Child Tasks

`boid task create` accepts YAML / JSON from stdin. Dependency resolution within the same batch using refs is handled server-side.

```bash
boid task create <<YAML
title: Task title
behavior: <key from task_behaviors in project.yaml, or omit>
parent_id: ${BOID_TASK_ID}
description: |
  Implementation instructions for this subtask. Describe what to build and how in detail.
auto_start: true
YAML
```

stdout returns `task created: <id> (<status>)`, which you can capture in a shell variable for monitoring:

```bash
CHILD_A=$(boid task create <<YAML | awk '{print $3}'
title: ...
parent_id: ${BOID_TASK_ID}
auto_start: true
YAML
)
```

### Required fields

- `title`: required.
- `parent_id: ${BOID_TASK_ID}`: required to maintain the parent-child relationship. Omitting this creates an independent task outside the supervisor's monitoring scope. `$BOID_TASK_ID` is provided as an environment variable (also available from `~/.boid/context/task.yaml`'s `id` field).

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

Settings like interactive / model / readonly are governed by the behavior template (`task_behaviors` in project.yaml). The plan itself switches between interactive consultation and autonomous decision-making by reading the `BOID_INTERACTIVE` environment variable, so there is no need to split "interactive plan" and "autonomous plan" into separate behaviors.

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

## When to Call notify

### Interactive mode

- **Plan-approval notify (conditionally required)** - Before creating one or more child tasks, present the full plan (list of children / behavior / order / estimated risks) and obtain approval. You may omit the approval notify if *any of the following* applies:
  - The user's request is already specific enough that there is little room for interpretation (e.g., "fix this line in this file like this", "rename `xxx` to yyy" - cases where the child task title/description is just a transcription of the request)
  - There is only one child task and the behavior and granularity are obvious from the request

  When in doubt, err on the side of notifying

- When half or more of the children are aborted, or when the plan strategy needs to change
- When the hard cap is reached (20 children / 12 hours)
- When an unexpected fact emerges from a child's artifact and the remaining plan needs to be revised

### Autonomous mode

Do not call notify. When stuck, write the situation to the artifact and exit (the user notices via task list and reopens with new instructions).

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

## User Notification (boid task notify)

When user judgment is needed, calling `boid task notify` executes the `notify.command` in `~/.config/boid/config.yaml`.

```bash
boid task notify ${BOID_TASK_ID} --message "Please decide how to incorporate the review feedback for PR #284"
```

The notification script receives `BOID_TASK_ID` / `BOID_PROJECT_ID` / `BOID_MESSAGE` / `BOID_TASK_URL` (a clickable link if `web.public_url` is set in config) as environment variables.

### Interactive mode only - wait in the session

Only call notify in **interactive mode (`BOID_INTERACTIVE=true`)**. When stuck in autonomous mode, do not notify; write the situation to the artifact and exit (the user notices via task list and reopens with new instructions).

In interactive mode, immediately after calling notify, output the question body (options / decision material / context) to the session and wait for the user's response:

```bash
boid task notify ${BOID_TASK_ID} --message "..."
echo "Decision needed:"
echo "  A. Proceed with approach ..."
echo "  B. Proceed with approach ..."
echo "  C. Present an alternative"
# The agent waits for user input here
```

The question content stays in the session transcript, so the user can read and respond via the Web UI session viewer (boid has no mechanism for storing question history).

### Notification semantics

- Each call triggers one notification. Multiple calls within a task are fine (calls = notifications)
- Do not call notify for progress reports (child status is visible in task list / Web UI)
- Call it for **decision branches** (which approach to take) and **pre-approval** (before running the full plan)
- Call it once when you reach a point where you cannot proceed without the user

## Exit Handling (required)

When all children have reached `done` / `aborted`, the plan agent **must execute exactly one of the following**.
Do not let the session hang without action (= users cannot notice the plan has completed unless they get a notify):

- **A. Autonomous exit**: Execute `boid job done "$BOID_JOB_ID" --exit-code 0`
- **B. Exit-confirmation notify**: Ask the user via `boid task notify` whether to close the session,
  wait for the response, then execute A when the user says OK

### Autonomous mode (`BOID_INTERACTIVE` unset / 0)

Always execute A (do not notify). Even when stuck, write the situation to the artifact then exit with A.

### Interactive mode (`BOID_INTERACTIVE=1`)

Execute A if **all** of the following are met; otherwise B:

1. All children are `done` / `aborted` with no remaining supervisor work
2. The user's most recent message was a closing response such as "ok", "got it", "thanks", and not a new request or question
3. There are no unanswered notifies (the user has not yet replied)
4. Your last message was a completion report with little room for the user to ask follow-up questions

When in doubt, choose B. Leaving the session open without doing anything is forbidden - always choose A or B.

#### Example of B (exit-confirmation notify)

```bash
boid task notify ${BOID_TASK_ID} --message "All child tasks complete. Confirming whether to close the session"
echo "All child tasks (child A: done, child B: done) have completed."
echo "Decision needed:"
echo "  A. Mark as complete and close the session (boid job done)"
echo "  B. Request additional work"
# Wait for user response
```

When the user responds A, execute `boid job done "$BOID_JOB_ID" --exit-code 0`.
When the user responds B, proceed with additional supervisor work.

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
parent_id: ${BOID_TASK_ID}
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
parent_id: ${BOID_TASK_ID}
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
parent_id: ${BOID_TASK_ID}
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
