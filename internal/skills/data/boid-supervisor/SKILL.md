---
name: boid-supervisor
description: Runs a supervisor task (readonly orchestrator) for the boid orchestrator.
  Reads task.yaml + instructions.yaml, creates child executor tasks via
  `boid task create`, monitors them in order, integrates results per the active
  instruction, and consults the user via `boid task notify --ask` when needed.
---

# boid Supervisor

A supervisor task **triages** a request, creates child executor tasks, and **monitors them** until completion. The supervisor is **readonly** — it reads the working tree and runs `git` queries but never edits project files. Implementation always happens in child executor tasks.

> **Your tools work — do not invent an I/O failure.** Empty or odd output is
> normal (`git status` on a clean tree is empty; a command that matched nothing
> prints nothing; the interactive harness can render a result a beat late).
> **Never** halt or escalate with "no command output is reaching me" / "the tool
> channel is broken" — that is a known confabulation that has wasted whole
> dispatches while commands were in fact returning output. If output looks empty,
> re-run that one command with `echo "RC=$?"` markers or write-to-file + Read;
> otherwise just proceed. Reserve `notify --ask` for real task blockers, never for
> suspected I/O trouble. Do not run "is my I/O working?" probe commands.

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

### Overriding the Behavior's `default_instruction` (partial)

A behavior's `default_instruction` in `project.yaml` carries `type` / `agent` / `name` / `message` / `model` for the child agent. By default the child task uses it verbatim. You can override **individual fields** without restating the whole instruction by passing a 1-entry `instructions:` array; non-empty fields win, empty fields inherit from `default_instruction`. (Implemented in `MergeDefaultInstructions` — `internal/orchestrator/payload_merge.go`.)

```yaml
title: heavier-than-usual refactor
behavior: executor
auto_start: true
instructions:
  - model: claude-opus-4-7   # message / agent / type は default から継承
```

Rules:

- 1-entry override + non-nil `default_instruction` → per-field merge (this case).
- 2+ entries → complete replacement (caller is intentionally rewriting the instruction history).
- empty / omitted `instructions` → use `default_instruction` as-is (the common case).

**Use this sparingly — only when the user explicitly asks for an override.** The project's `default_instruction` exists because the project owner picked it as the right default; silently swapping `model` or `message` because you, as the supervisor, decided it'd "probably be better" undermines that and surprises the user. Reach for partial override **only when** the active instruction (or a user reply via `notify --ask`) explicitly tells you to (e.g. "use opus for the heavy refactor", "rewrite the message to focus on X"). When in doubt, ask via `notify --ask` before overriding.

## Monitoring Children

> **Critical — do not poll in the foreground.** The agent harness **blocks
> foreground `sleep`**: a `while true; …; sleep N; done` loop run as a Bash tool
> call returns `<tool_use_error>Blocked: sleep …` and never executes. Polling by
> hand across turns (firing `boid task show` repeatedly) collides with the
> harness's tool-call scheduler and yields `<tool_use_error>Cancelled: parallel
> tool call …`. **Empty / cancelled / blocked tool results are transient harness
> artifacts — never read them as a sandbox failure or as evidence a child is
> stuck.** The correct way to wait is a single background watcher: arm it, then
> stop generating.

Arm one **Monitor** per child. The watch script polls the child's status and
emits one line **on every status change** — so you wake for `awaiting` mid-flight
as well as for the terminal `done` / `aborted` — and exits when the child is
terminal:

```bash
# Monitor tool — command:
CHILD="<child-id>"
prev=""
while true; do
  st=$(boid task show "$CHILD" --field status 2>/dev/null || echo "")
  if [ -n "$st" ] && [ "$st" != "$prev" ]; then
    echo "child $CHILD -> $st"        # one line per change → one notification
    prev="$st"
  fi
  case "$st" in done|aborted) exit 0 ;; esac  # terminal → end the watch
  sleep 30                                      # sleep is fine *inside* Monitor
done
```

Give the Monitor a `description` like `child <short-id> status changes` and a
`timeout_ms` matched to scale (default 300000; up to 3600000 for long builds), or
`persistent: true` for very long children. `sleep` **inside** the Monitor script
is fine — the script runs in the background; only a *foreground* `sleep` is
blocked. After arming, **stop generating**; you are notified on each event.

On each notification, branch on the reported status:

- `awaiting` — child called `notify --ask` (mid-flight question). Handle it (see
  [Handling Awaiting](#handling-awaiting)), then **keep waiting**: `boid task
  answer` / `reopen` do not terminate you, so the same Monitor stays armed and
  wakes you on the next change — no re-arm needed.
- `done` — child called `notify --done` (success self-report). Verify and
  integrate (see [Handling Done](#handling-done)); the Monitor has already exited.
- `aborted` — child called `notify --fail` or `action send --type abort`. See
  [Handling Aborted](#handling-aborted); the Monitor has already exited.

**Re-arm only when you yourself paused.** If you escalate the child's question up
with your own `notify --ask`, the daemon SIGTERMs your runtime and the Monitor
dies with it. On resume, arm a fresh Monitor for the child before stopping again.

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

## Reporting Your Own Done (the daemon verifies — do not fabricate)

When **you** finish and report up with `notify --done`, the daemon **rejects
fabricated or premature reports**. A rejected `notify --done` returns an error in
your Bash tool result and does **not** end your session — fix the real state and
report again. Two rules:

1. **Never report done while a child is open.** Wait until every child you created
   is `done` / `aborted`, confirmed by an actual Monitor `done` event or a real
   `boid task show "$child" --field status` result — not by assuming the wait
   finished. The daemon rejects `notify --done` while any child is still open
   (`cannot report done: N child task(s) are still open`). This is the most common
   failure: after delegating, agents *narrate* the child finishing instead of
   actually waiting. Arm the Monitor, **stop generating**, and resume on the real
   event.

2. **Never cite a commit/branch you have not seen in real git output.** If your
   done involves a release (merge/push), record it in
   `payload.artifact.report.release` from the **actual** command output, then
   report done:

   ```bash
   merged=$(git rev-parse HEAD)                 # the real merged commit
   git push origin "$merged:$BRANCH"            # real push
   boid task update "$BOID_TASK_ID" --payload-file - <<EOF
   artifact:
     report:
       release:
         commit: "$merged"
         branch: "$BRANCH"
         pushed: true
   EOF
   boid task notify "$BOID_TASK_ID" --done "Released $merged to $BRANCH (PR updated)."
   ```

   The daemon verifies `release.commit` exists in the repo (and, when
   `pushed: true`, that `origin/<branch>` matches). A hash you invented to fill the
   report — a plausible-looking object name you never saw in a tool result — is
   rejected (`reported release commit ... does not exist`).

**If you did not actually run the merge/push and see its output, you have not done
it — do not claim it.** Report the true state instead: escalate the blocker via
your own `notify --ask`, or `--done` with an honest description of what remains.
Inventing a successful-looking report is the single failure mode this skill most
needs you to avoid.

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

1. **Silent exit** — `claude` exits without issuing `notify --ask`, leaving the child in `executing` with no live job (`job.status != running`).
2. **PTY hang** — `claude` is still running (`job.status == running`) but the PTY is waiting for input and `transcript.log` has had no new writes for a long time (`transcript_idle_seconds` large).

Detect this **in the background, inside the same Monitor watch script** — never by
foreground polling (foreground `sleep` is blocked; see [Monitoring Children](#monitoring-children)).
Augment the watch loop to emit a one-shot `stuck` event when the child sits in
`executing` with an idle / exited job past a threshold:

```bash
# Monitor tool — command (extends the watch loop from "Monitoring Children"):
CHILD="<child-id>"; IDLE_MAX=600       # 300 fast-iter / 1800 long-build
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

Threshold guidance for `transcript_idle_seconds`:
- Default: **600** (10 min) — covers most executor tasks
- Fast iteration: **300** (5 min) — when the executor should be actively writing
- Long build / slow network: **1800** (30 min) — when legitimate pauses are expected

Note: `boid task notify --progress` does **not** update `transcript.log` (it goes through the broker, not the PTY). Only actual agent output advances the mtime.

**On a `stuck` event, confirm before acting.** Re-read the child's last job/action
and git state **once** (a single command — not a foreground poll loop). Empty,
cancelled, or blocked tool output is never itself evidence of a stuck child; if a
read comes back empty, re-run that one command rather than concluding failure.
Only after a real signal, decide:

- **Reopen with a status check** — `boid task reopen "$child" -m "Status check: where are you? Emit notify --ask 'done_request: ...' if you finished, or describe what you need."`
- **Abort** when clearly unrecoverable
- **Escalate up** when you cannot decide (your own `notify --ask`; re-arm the Monitor on resume)

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
