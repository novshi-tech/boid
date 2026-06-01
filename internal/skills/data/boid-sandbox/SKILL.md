---
name: boid-sandbox
description: Behavior-aware dispatch shim for the boid orchestrator sandbox.
  Reads `~/.boid/context/task.yaml` and routes the agent to the canonical
  per-behavior skill (`/boid-supervisor` or `/boid-executor`). Kept for
  backward compatibility with older `task_behaviors` configurations that
  reference `/boid-sandbox` directly.
---

# boid Sandbox (dispatch shim)

This skill is a **shim**. The canonical agent-side skills are now per-behavior:

- **`/boid-supervisor`** — readonly orchestrator. Triages a request, creates child executor tasks, monitors them, integrates results.
- **`/boid-executor`** — writable implementer. Reads `task.yaml`, makes the change, commits, exits.

When a project's `task_behaviors[*].default_instruction.message` still says "follow `/boid-sandbox`" (older projects), read `~/.boid/context/task.yaml` and follow the matching canonical skill instead.

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

Reserve `notify --ask` for genuine task blockers (a missing requirement, a real
decision for your owner) — never for "I think my I/O is broken." Do not run
"is my I/O working?" probe commands; just do the task.

## Dispatch Table

| `task.yaml` `behavior` field | Follow |
|---|---|
| `supervisor` (canonical) or `plan` (alias) | [`/boid-supervisor`](../boid-supervisor/SKILL.md) |
| `executor` (canonical) or `dev` (alias) | [`/boid-executor`](../boid-executor/SKILL.md) |
| anything else | Follow the project's `default_instruction.message` for that behavior; fall back to `/boid-executor` if the behavior is clearly writable, `/boid-supervisor` if it is clearly readonly |

The alias map (`plan → supervisor`, `dev → executor`) is applied by the daemon when loading `project.yaml`, so `task.yaml.behavior` is usually already canonical by the time you read it.

## Why This Skill Still Exists

Projects that pinned `/boid-sandbox` in their `default_instruction.message` (or kit) before the canonical skills landed keep pointing here. Rather than break them, this shim routes the agent to the right place. New projects should reference `/boid-supervisor` or `/boid-executor` directly so the dispatch step is not needed.

## References

- [references/data-model.md](references/data-model.md) — schema for the `~/.boid/context/*.yaml` files that every behavior reads (task / instructions / payload / environment). Linked from `/boid-supervisor` as well; kept here because it is shared across all behaviors.
