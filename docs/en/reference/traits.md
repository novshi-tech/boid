# Payload trait reference

The keys you can place at the top level of a task payload (the *traits*), and how each one affects state transitions.

[Concepts / Payload and traits](../guide/concepts.md#payload-and-traits) gives a short introduction. This page is the canonical reference: every defined trait, the merge modes, and the conditions the state machine actually evaluates.

## Background

- A payload is the JSON document that grows as the task progresses.
- Top-level keys of the payload are called *traits*.
- Which traits a hook / gate may read and write is declared in [`kit.yaml`](../kit-authoring/overview.md) under `traits.consumes` / `traits.produces`.
- Updates flow through payload patches (a JSON document with a `payload_patch` wrapper) emitted by handlers — see [Handler script protocol](handler-contract.md).

## Defined traits

Three traits drive `boid`'s state-transition rules.

| Trait | Producible by | What the state machine looks at |
|---|---|---|
| `artifact` | hooks / gates that produce | Whether the key is present (signals "executing complete") |
| `tasks` | hooks / gates that produce | Whether the value is a non-empty array (signals "executing complete" for plan-style tasks) |
| `verification` | hooks / gates that produce (shared mode) | The `findings` and `source_state` of each subkey |

`artifact` and `tasks` are symmetrical, both meaning "the work expected from `executing` is in place".

### `artifact`

Where the executing hook writes its results. The internal shape is up to the project / kit, except that `artifact.children.*` is reserved by `boid` and a handler that tries to write under it gets an error.

State-machine view:

- A non-null `artifact` key in the payload counts as "executing complete".
- Either `artifact` or `tasks` being present satisfies the completion condition.

### `tasks`

The plan-style counterpart to `artifact`. Used by behaviors such as `plan` that emit a list of tasks instead of a single artifact.

State-machine view:

- `tasks` counts as "executing complete" only when it is a **non-empty array**.
- Empty arrays and `null` do not count (where `artifact` checks presence, `tasks` checks element count).

### `verification`

Where review-style hooks and gates write findings. Its merge mode is **shared**: the runtime automatically wraps each handler's payload patch under that handler's ID.

The shape of `verification` in the persisted payload:

```json
{
  "verification": {
    "<handler-id>": {
      "source_state": "executing",
      "findings": [
        {
          "message": "...",
          "status": "open",
          "severity": "fatal"
        }
      ]
    },
    "<another-handler-id>": {
      "source_state": "verifying",
      "findings": [...]
    }
  }
}
```

Each subkey is one handler's write area. When that handler emits the patch again, only its own subkey is overwritten — other handlers' writes are preserved (that is what *shared* means).

The handler itself emits a flat patch and does not have to know about the wrapping:

```json
{
  "payload_patch": {
    "verification": {
      "findings": [...]
    }
  }
}
```

`source_state` is also added automatically: `boid`'s coordinator stamps the patch with the **task's current status** (e.g. `executing` / `verifying` / `reworking`) before merging.

#### Finding shape

Each element of the `findings` array has:

| Key | Type | Role |
|---|---|---|
| `message` | string | Free-form description of the issue. The rework hook reads this. |
| `status` | string | `open` (unresolved) or `resolved`. |
| `severity` | string | `normal` (default) or `fatal`. An open `fatal` finding aborts the task immediately. |

#### How the state machine reads it

The auto-transitions for verifying / reworking only look at **subkeys whose `source_state` matches the relevant state**:

- An open finding sourced from `executing` → `executing → reworking`.
- An open finding sourced from `verifying` → `verifying → reworking`.
- All findings sourced from `reworking` resolved → `reworking → verifying`.
- Any open finding with `severity: fatal`, in any source state → `aborted`.

This per-source filtering is intentional: a gate that writes a `verifying`-sourced finding (such as `mergeable-check`) can coexist with `reworking`-sourced findings written by the rework agent. The exit from `reworking` only checks `reworking`-sourced findings, so a verifying-sourced one cannot deadlock the rework loop.

## Computed values

### `lifecycle`

Values derived automatically from the task's history, used only to evaluate transitions. **Lifecycle is not stored in the payload** — the state machine injects it as a virtual trait at evaluation time.

Available fields:

| Field | Type | Meaning |
|---|---|---|
| `lifecycle.rework_count` | int | Number of times the task has entered `reworking`. Used for the abort-on-overlimit rule. |
| `lifecycle.executed` | bool | Whether at least one hook has run during `executing`. |

A handler that emits a payload patch writing to `lifecycle` accomplishes nothing — the auto-derived value overwrites it. Listing `lifecycle` under a hook's `traits.produces` is meaningless.

### Reserved keys

- **`artifact.children.*`** is reserved as the view area where a parent task can read its children's state. `boid` itself maintains it during evaluation; a handler that tries to write here gets an error.

## Not a payload trait

### `instructions`

In an earlier design, instructions lived inside the payload. They have since moved out: instructions are now a **top-level field on the task** (`Task.instructions`) and are not part of the payload at all. Trying to write `instructions` inside the payload produces an error.

For the structure of an `Instruction`, see [`project.yaml` reference / Instruction](project-yaml.md#instruction).

## Merge modes

Each trait has a defined merge mode that decides how a handler's payload patch combines with the existing payload.

| Trait | Mode | Meaning |
|---|---|---|
| `verification` | **shared** | Wrap under the handler ID. Multiple handlers' writes coexist as separate subkeys. |
| `artifact`, `tasks`, anything else | **exclusive** | Last writer wins. The handler's value replaces the existing same-key value. |

Shared mode is how multiple handlers can write to the same trait in parallel without overwriting each other. `verification` is shared because review-style handlers are expected to be plural.

## Declaring traits on a handler

[`kit.yaml`](../kit-authoring/overview.md) declares which traits a hook or gate reads and writes:

```yaml
hooks:
  - id: my-hook
    on: [executing]
    traits:
      consumes: [instructions]      # values read (delivered through TaskJSON)
      produces: [artifact]          # values written (anything else in the patch is dropped)
```

### Optional consumes (`?` suffix)

A trailing `?` on a `consumes` entry marks the trait optional, so the handler runs even if it is absent.

```yaml
traits:
  consumes: [artifact?, verification?]
```

`?` is meaningful only on `consumes`; do not add it to `produces`.

### Traits not in `produces` are dropped

If a handler's payload patch contains a trait not listed in `produces`, `boid` logs a warning and **drops just that trait**. The rest of the patch is still applied.

## Related documents

- [Concepts / Payload and traits](../guide/concepts.md#payload-and-traits) — short introduction.
- [State machine](../guide/state-machine.md) — which trait writes drive which transitions.
- [Handler script protocol](handler-contract.md) — how to emit payload patches.
- [Kit authoring overview](../kit-authoring/overview.md) — declaring `traits.consumes` / `produces`.
- [`project.yaml` reference / Instruction](project-yaml.md#instruction) — the shape of `instructions`.
