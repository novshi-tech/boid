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
