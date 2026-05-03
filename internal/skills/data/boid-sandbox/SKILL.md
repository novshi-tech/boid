---
name: boid-sandbox
description: Runs tasks in the boid orchestrator sandbox environment.
  Used when a task context exists in ~/.boid/context/.
  Reads task state and instructions from context files and performs work according to the current state.
---

# boid Sandbox

## Context

| File | Contents |
|---------|------|
| `~/.boid/context/task.yaml` | Task ID, title, description, status, behavior |
| `~/.boid/context/instructions.yaml` | Instructions addressed to you (array) |
| `~/.boid/context/payload.yaml` | Full payload (existing artifacts, verification results) |
| `~/.boid/context/environment.yaml` | Sandbox constraints (RO/RW, network, tools) |

Start by reading `task.yaml` and `instructions.yaml` to understand the task.

## Output

Deliver results via the route appropriate for the behavior (plan uses the `boid task create` builtin, dev follows the dev-pr-flow skill, etc.).
payload_patch.json is an internal boid implementation detail and can normally be ignored.

## State and Actions

Check the current state in the `status` field of `task.yaml`.
See [state-machine.md](references/state-machine.md) for what to do in each state.

## Rules

- Do not include the `instructions` trait in output (read-only)
- Follow the constraints in `environment.yaml`
- When `environment.yaml` has `worktree: true`, always git commit your work before exiting (the worktree is deleted when the task completes)
