# task_behaviors migration guide: free naming and `readonly` defaults

> Changes introduced in Track A2 (2026-06-17).

> **Phase 2 note (v0.0.12)**: The `worktree: true` shown in the yaml examples below has been retired in branch-policy-simplification Phase 2. Existing `worktree:` lines are silently ignored for BC. See [Concepts / Worktree](../guide/concepts.md#worktree) for the current model.

## Overview

Previously, only two map keys — `supervisor` and `executor` — were recognised as
valid canonical names in `task_behaviors`. Track A2 removes this restriction so
that **any name (free naming)** can be used.

Additionally:

- **`readonly` now defaults to `true`** (fail-safe). Behaviors that need write
  access must declare `readonly: false` explicitly.
- **`default_task_behavior`** is a new top-level key that names the behavior
  `boid task create` uses when `--behavior` is omitted.

---

## `supervisor` / `executor` are now deprecated

These names continue to work, but are **deprecated**. A single WARN log entry is
emitted when `ReadProjectMetaWithKits` runs (daemon start or project reload).

To suppress the warning, set `BOID_NO_DEPRECATION_WARN=1`.

---

## Migration steps

### Before (old canonical names)

```yaml
# .boid/project.yaml
id: my-project
name: My Project
worktree: true

task_behaviors:
  supervisor:
    default_instruction:
      agent: claude-code
      message: |
        ...
  executor:
    default_instruction:
      agent: claude-code
      message: |
        ...
```

### After (free naming)

```yaml
# .boid/project.yaml
id: my-project
name: My Project
worktree: true

default_task_behavior: plan   # ← new: default for boid task create

task_behaviors:
  plan:                        # ← renamed from "supervisor"
    readonly: true             # ← explicit (true is already the default)
    default_instruction:
      agent: claude-code
      message: |
        ...
  dev:                         # ← renamed from "executor"
    readonly: false            # ← required for writable behaviors
    default_instruction:
      agent: claude-code
      message: |
        ...
```

---

## How `readonly` defaults changed

| Situation | Before (Phase 3-1) | After (Track A2) |
|---|---|---|
| `supervisor` (no explicit value) | readonly = true (automatic) | readonly = true (same as the default) |
| `executor` (no explicit value) | readonly = false (automatic) | **readonly = false (compat override, WARN emitted)** |
| Non-canonical name (no explicit value) | readonly = false | **readonly = true (fail-safe)** |
| Any name, `readonly: false` explicit | — | readonly = false |
| Any name, `readonly: true` explicit | — | readonly = true |

### Keeping `executor` without renaming

Adding `readonly: false` suppresses the deprecation warning:

```yaml
task_behaviors:
  executor:
    readonly: false   # ← add this line to silence the WARN
    default_instruction:
      ...
```

---

## Setting `default_task_behavior`

When `behavior` is omitted from `boid task create` (CLI or Web UI), the daemon
uses the behavior named by `default_task_behavior`.

```yaml
default_task_behavior: plan
```

**Fallback when unset:**

1. If `task_behaviors` contains `supervisor`, it is used implicitly (WARN emitted).
2. If `supervisor` is also absent, `boid task create` returns an error.

---

## Common migration patterns

### Simple rename + default

```yaml
default_task_behavior: plan

task_behaviors:
  plan:          # formerly supervisor
    readonly: true
    ...
  dev:           # formerly executor
    readonly: false
    ...
```

### Multiple root templates side by side

```yaml
default_task_behavior: dev

task_behaviors:
  plan:
    readonly: true
    default_instruction: { agent: claude-code, message: "Plan the work..." }
  dev:
    readonly: false
    default_instruction: { agent: claude-code, message: "Implement the feature..." }
  review:
    readonly: true
    default_instruction: { agent: claude-code, message: "Review the PR..." }
```

Pass `--behavior review` to `boid task create` to select any named template
explicitly. When omitted, `default_task_behavior: dev` applies.
