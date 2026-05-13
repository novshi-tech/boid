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
- **`/boid-executor`** — writable implementer. Reads `task.yaml`, makes the change, runs the project's release step, exits.

When a project's `task_behaviors[*].default_instruction.message` still says "follow `/boid-sandbox`" (older projects), look at the `behavior` field in `~/.boid/context/task.yaml` and follow the matching canonical skill instead.

## Dispatch Table

| `task.yaml` `behavior` field | Follow |
|---|---|
| `supervisor` (canonical) or `plan` (alias) | [`/boid-supervisor`](../boid-supervisor/SKILL.md) |
| `executor` (canonical) or `dev` (alias) | [`/boid-executor`](../boid-executor/SKILL.md) |
| anything else | Follow the project's `default_instruction.message` for that behavior; fall back to `/boid-executor` if the behavior is clearly writable, `/boid-supervisor` if it is clearly readonly |

The alias map (`plan → supervisor`, `dev → executor`) is also applied by the daemon when loading `project.yaml`, so `task.yaml.behavior` is usually already in canonical form by the time you read it.

## Context Files (common to all behaviors)

| File | Contents |
|---------|------|
| `~/.boid/context/task.yaml` | Task ID, title, description, status, behavior |
| `~/.boid/context/instructions.yaml` | Instructions addressed to you (array; last element is active) |
| `~/.boid/context/payload.yaml` | Full payload (existing artifacts, verification results) — read-only |
| `~/.boid/context/environment.yaml` | Sandbox constraints (RO/RW, network, tools) |

Start by reading `task.yaml`, dispatch to the right per-behavior skill, then read `instructions.yaml` per that skill's protocol.

## Why this skill still exists

Projects that pinned `/boid-sandbox` in their `default_instruction.message` (or kit) before the canonical skills landed will keep pointing here. Rather than break them, this shim sends the agent to the right place. New projects should reference `/boid-supervisor` or `/boid-executor` directly so the dispatch step is not needed.

See [state-machine.md](references/state-machine.md) for the shared state machine and [data-model.md](references/data-model.md) for the context file schema.
