# Payload trait reference

The keys you can place at the top level of a task payload (the *traits*), and how each one affects state transitions.

[Concepts / Payload and traits](../guide/concepts.md#payload-and-traits) gives a short introduction. This page is the canonical reference.

## Background

- A payload is the JSON document that grows as the task progresses.
- Top-level keys of the payload are called *traits*.
- Which traits a hook may read and write is declared on the hook itself, in [`project.yaml`](project-yaml.md)'s `task_behaviors.<name>.hooks[]` entries, under `traits.consumes` / `traits.produces`. Hooks are always authoritative in `project.yaml` — a kit never provides hooks, so `kit.yaml` has no equivalent declaration.
- Updates flow through payload patches (a JSON document with a `payload_patch` wrapper) emitted by hooks — see [Hook script protocol](hook-contract.md).

## Defined traits

The following traits are defined for task payloads. The state machine's auto-transitions are *not* driven by trait values directly; they fire on hook completion (`boid job done`).

| Trait | Producible by | Merge mode | Contents |
|---|---|---|---|
| `artifact` | hooks | exclusive | Free-form map for the task's output (commit, PR URL, files changed, ...). |
| `verification` | hooks | shared | Results from verification hooks, merged by handler ID sub-key. |
| `awaiting` | boid core | exclusive | Persistent Q&A state set by `boid task ask` (blocking RPC) or `boid task notify --ask`. See [awaiting trait](#awaiting-trait). |

### `artifact`

Where the executing hook writes its results. The internal shape is up to the project / kit, except that `artifact.children.*` is reserved by `boid` (used as a view from a parent task into its children) and a hook that tries to write under it gets an error.

### `verification`

Written by hooks that perform verification steps. Unlike `artifact`, the merge mode is **shared**: each hook writes under its own handler-ID sub-key, so multiple verification hooks can run in parallel without overwriting each other's results.

### `awaiting` trait

Set automatically by `boid` when `boid task ask` (blocking RPC) or `boid task notify --ask` is called. The `boid task ask` flow holds the agent's broker connection open and routes the reply back over the same socket via an in-memory registry; `notify --ask` only flips the task into `awaiting` and exits the agent — the daemon no longer dispatches a resume hook on answer (session-id resume was removed). Prefer `boid task ask` for any real Q&A.

Fields:

| Field | Type | Set by | Role |
|---|---|---|---|
| `question` | string | boid core | Human-readable question text shown to the user. |
| `question_id` | string | boid core | UUID identifying this Q&A turn. |
| `pending_answer` | string | boid core | Legacy `notify --ask` reply slot. Unused by the `boid task ask` path (answers are delivered in-memory). |

The `awaiting` trait is managed exclusively by `boid` core and the `ApplyAction("ask"/"answer")` path. Hooks must not write to it directly. Legacy records may still carry `session_id` / `mode` fields — the deserializer silently ignores them (they were removed from the struct).

### Subtask creation

Supervisor behaviors no longer emit subtasks via a payload trait. Instead the hook calls the `boid task create` builtin directly. See the [`/boid-task` SKILL — Supervisor Mode](../../../internal/skills/data/boid-task/SKILL.md) for the typical shape.

## Computed values

### `lifecycle`

Values derived automatically from the task's history, used only to evaluate transitions. **Lifecycle is not stored in the payload** — the state machine injects it as a virtual trait at evaluation time.

Available fields:

| Field | Type | Meaning |
|---|---|---|
| `lifecycle.executed` | bool | `true` when the hook job completed successfully in the current dispatch cycle. This is the primary trigger for auto-advance rules. |
| `lifecycle.done` | object | Set when `boid task notify --done` was called in the current executing cycle. Contains `message`. Drives the `executing→done` auto-transition (combined with `lifecycle.executed`). |
| `lifecycle.fail` | object | Set when `boid task notify --fail` was called in the current executing cycle. Contains `message`. Drives the `executing→aborted` auto-transition (takes precedence over `lifecycle.done`). |
| `lifecycle.abort.code` | string | Reason code captured when the task aborted. |
| `lifecycle.abort.message` | string | Human-readable abort message. |

Auto-advance rules evaluated on hook completion:

1. `executing→aborted` when `lifecycle.executed && lifecycle.fail`
2. `executing→done` when `lifecycle.executed && lifecycle.done`
3. `executing→done` when `lifecycle.executed` only (legacy hook path, no explicit notify)

A hook that emits a payload patch writing to `lifecycle` accomplishes nothing — the auto-derived value overwrites it. Listing `lifecycle` under a hook's `traits.produces` is meaningless.

### Reserved keys

- **`artifact.children.*`** is reserved as the view area where a parent task can read its children's state. `boid` itself maintains it during evaluation; a hook that tries to write here gets an error.

## Not a payload trait

### `instructions`

Instructions are not a payload trait. They live in the top-level `Task.Instructions` array on the task itself; the last element is the active one, and `boid task reopen <id> --message "..."` appends a new entry.

For the shape of an `Instruction`, see [`project.yaml` reference / Instruction](project-yaml.md#instruction).

## Merge modes

There are three merge modes:

| Mode | Meaning |
|---|---|
| **exclusive** | Last writer wins. The hook's value replaces the existing same-key value. |
| **shared** | The hook's value is merged at the handler-ID sub-key level, so multiple hooks can write without overwriting each other. |
| **default** | Falls back to **exclusive** unless overridden. |

Merge mode per trait:

| Trait | Mode |
|---|---|
| `verification` | **shared** (merges by handler ID sub-key) |
| `artifact`, anything else | **exclusive** |

When multiple hooks run in parallel, give each one its own sub-key (e.g. `artifact.<my-hook-id>`) to avoid collisions.

## Declaring traits on a hook

A hook's entry under [`project.yaml`](project-yaml.md)'s `task_behaviors.<name>.hooks[]` declares which traits it reads and writes:

```yaml
task_behaviors:
  executor:
    hooks:
      - id: my-hook
        traits:
          consumes: [artifact?]    # values read (delivered through TaskJSON)
          produces: [artifact]     # values written (anything else in the patch is dropped)
```

### Optional consumes (`?` suffix)

A trailing `?` on a `consumes` entry marks the trait optional, so the hook runs even if it is absent.

```yaml
traits:
  consumes: [artifact?]
```

`?` is meaningful only on `consumes`; do not add it to `produces`.

### Traits not in `produces` are dropped

If a hook's payload patch contains a trait not listed in `produces`, `boid` logs a warning and **drops just that trait**. The rest of the patch is still applied.

## Related documents

- [Concepts / Payload and traits](../guide/concepts.md#payload-and-traits) — short introduction.
- [State machine](../guide/state-machine.md) — how hook completion drives transitions.
- [Hook script protocol](hook-contract.md) — how to emit payload patches.
- [`project.yaml` reference](project-yaml.md) — declaring `traits.consumes` / `produces` on a hook.
- [`project.yaml` reference / Instruction](project-yaml.md#instruction) — the shape of `instructions`.
