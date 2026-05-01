# Payload trait reference

The keys you can place at the top level of a task payload (the *traits*), and how each one affects state transitions.

[Concepts / Payload and traits](../guide/concepts.md#payload-and-traits) gives a short introduction. This page is the canonical reference.

## Background

- A payload is the JSON document that grows as the task progresses.
- Top-level keys of the payload are called *traits*.
- Which traits a hook / gate may read and write is declared in [`kit.yaml`](../kit-authoring/overview.md) under `traits.consumes` / `traits.produces`.
- Updates flow through payload patches (a JSON document with a `payload_patch` wrapper) emitted by handlers — see [Handler script protocol](handler-contract.md).

## Defined traits

Two traits live in the payload. The state machine's auto-transitions are *not* driven by trait values directly; they fire on hook completion (`boid job done`) and on gate exit codes.

| Trait | Producible by | Contents |
|---|---|---|
| `artifact` | hooks / gates | Free-form map for the task's output (commit, PR URL, files changed, ...). |
| `tasks` | hooks / gates | Subtask array emitted by planning behaviors. |

### `artifact`

Where the executing hook writes its results. The internal shape is up to the project / kit, except that `artifact.children.*` is reserved by `boid` (used as a view from a parent task into its children) and a handler that tries to write under it gets an error.

### `tasks`

The plan-style counterpart to `artifact`. Used by behaviors such as `plan` that emit a list of tasks; see the [boid-plan SKILL](../../../internal/skills/data/boid-plan/SKILL.md) for the array shape.

## Computed values

### `lifecycle`

Values derived automatically from the task's history, used only to evaluate transitions. **Lifecycle is not stored in the payload** — the state machine injects it as a virtual trait at evaluation time.

Available fields:

| Field | Type | Meaning |
|---|---|---|
| `lifecycle.abort.code` | string | Reason code captured when the task aborted. |
| `lifecycle.abort.message` | string | Human-readable abort message. |

A handler that emits a payload patch writing to `lifecycle` accomplishes nothing — the auto-derived value overwrites it. Listing `lifecycle` under a hook's `traits.produces` is meaningless.

### Reserved keys

- **`artifact.children.*`** is reserved as the view area where a parent task can read its children's state. `boid` itself maintains it during evaluation; a handler that tries to write here gets an error.

## Not a payload trait

### `instructions`

Instructions are not a payload trait. They live in the top-level `Task.Instructions` array on the task itself; the last element is the active one, and `boid task reopen <id> --message "..."` appends a new entry.

For the shape of an `Instruction`, see [`project.yaml` reference / Instruction](project-yaml.md#instruction).

## Merge modes

| Trait | Mode | Meaning |
|---|---|---|
| `artifact`, `tasks`, anything else | **exclusive** | Last writer wins. The handler's value replaces the existing same-key value. |

When multiple handlers run in parallel, give each one its own sub-key (e.g. `artifact.<my-handler-id>`) to avoid collisions.

## Declaring traits on a handler

[`kit.yaml`](../kit-authoring/overview.md) declares which traits a hook or gate reads and writes:

```yaml
hooks:
  - id: my-hook
    traits:
      consumes: [artifact?]    # values read (delivered through TaskJSON)
      produces: [artifact]     # values written (anything else in the patch is dropped)
```

### Optional consumes (`?` suffix)

A trailing `?` on a `consumes` entry marks the trait optional, so the handler runs even if it is absent.

```yaml
traits:
  consumes: [artifact?]
```

`?` is meaningful only on `consumes`; do not add it to `produces`.

### Traits not in `produces` are dropped

If a handler's payload patch contains a trait not listed in `produces`, `boid` logs a warning and **drops just that trait**. The rest of the patch is still applied.

## Related documents

- [Concepts / Payload and traits](../guide/concepts.md#payload-and-traits) — short introduction.
- [State machine](../guide/state-machine.md) — how hook completion and gate failures drive transitions.
- [Handler script protocol](handler-contract.md) — how to emit payload patches.
- [Kit authoring overview](../kit-authoring/overview.md) — declaring `traits.consumes` / `produces`.
- [`project.yaml` reference / Instruction](project-yaml.md#instruction) — the shape of `instructions`.
