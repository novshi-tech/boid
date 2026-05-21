# Hook script protocol reference

The complete I/O contract between `boid` and hook scripts.

The [Kit authoring overview](../kit-authoring/overview.md) summarises the protocol; this page is the canonical reference for inputs (stdin, environment, working directory), outputs (`payload_patch.json`, stdout, stderr), exit codes, and the data structures involved.

## Inputs

### stdin

When a hook is launched, stdin carries the entire task as a single JSON document (TaskJSON). The size is variable, so read until EOF before parsing.

The main fields of TaskJSON:

| Key | Type | Role |
|---|---|---|
| `id` | string | Task ID (UUID). |
| `project_id` | string | The owning project's ID. |
| `title` | string | Task title. |
| `description` | string | Free-form body. |
| `status` | string | Current state (`pending` / `executing` / ...). |
| `behavior` | string | Behavior name (`supervisor` or `executor`). |
| `traits` | list of string | Traits the behavior declared. |
| `readonly` | bool | Whether the sandbox is read-only (derived: `true` for supervisor, `false` for executor). |
| `worktree` | bool | Whether this task has a worktree (project-top `worktree:` flag combined with the behavior name). |
| `branch_prefix` | string | Worktree branch-name prefix (always `boid/` — no longer user-configurable). |
| `base_branch` | string | Worktree base branch (resolved from project-top `base_branch:` with `${TASK_REMOTE_ID}` / `${current_branch}` expansion). |
| `payload` | object | The full current payload — most hooks read from here. |
| `instructions` | map (role → Instruction) | Routed instructions; meaningful only to hooks declared `kind: agent`. |
| `auto_start` | bool | Whether the task was created with auto-start. |
| `parent_id` | string | Optional parent task ID. |
| `created_at` / `updated_at` | RFC3339 timestamp | Creation / update times. |

The complete shape lives in the `Task` type at [`internal/orchestrator/spec_types.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/spec_types.go).

### Environment variables

The hook runs with the following environment variables set:

| Variable | Role |
|---|---|
| `BOID_TASK_ID` | Current task ID (same value as TaskJSON's `id`). |
| `BOID_JOB_ID` | Current job ID (used by `boid job show <id>`). |
| `BOID_PROJECT_ID` | Project ID. |
| `BOID_BASE_BRANCH` | The task's `base_branch` (the PR target branch). Set for both root and child tasks. |
| `BOID_PARENT_BRANCH` | The parent task's HEAD branch. Empty for root tasks. Used by sub-supervisors (e.g. `git merge $BOID_PARENT_BRANCH`). |
| `HOME` | The sandbox home. |
| `PATH` | Inherited from the launcher; may be overridden by the kit's `env`. |

Any variables declared in the kit's `kit.yaml` are also exported.

### Working directory

- **Tasks with a worktree** (root case 2/3, or any child task) run with the cwd set to **the worktree root**.
- **Tasks without a worktree** (root case 1: `base_branch` matches the project HEAD, or executor in a project without `worktree:`) run with the cwd set to **the project root** (the directory containing `project.yaml`).

This means commands like `git`, `gh`, and language toolchains do not need explicit directory arguments.

### File system access

Hooks run inside the sandbox. They can read and write only inside the worktree (and nowhere if `readonly: true`). Paths declared in the kit's `additional_bindings` are mounted in addition. The host's home directory, SSH keys, and other projects are not visible.

## Outputs

To update the payload, a hook returns a **payload patch**. Two output paths are supported, with a defined priority.

### Path 1: `$HOME/.boid/output/payload_patch.json` (preferred)

Write the JSON document to `$HOME/.boid/output/payload_patch.json`. When the hook exits, the runtime reads this file and forwards it to `boid`.

If the file exists, stdout is ignored.

```bash
mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<'JSON'
{
  "payload_patch": {
    "artifact": { "result": "ok" }
  }
}
JSON
```

### Path 2: stdout (fallback)

Only when `payload_patch.json` is absent does stdout become the payload-patch source. It can span multiple lines, but it must be a single valid JSON document.

```bash
echo '{"payload_patch":{"artifact":{"result":"ok"}}}'
```

For new hooks, prefer the file path. Agent-style hooks (such as `claude-code`) print incidental output on stdout, so using the file path avoids accidental misparses.

### payload patch shape

The top level must be a `payload_patch` key. Its body is deep-merged into the current payload.

```json
{
  "payload_patch": {
    "artifact": {
      "<key>": "<value>"
    }
  }
}
```

In practice the only trait a hook writes is `artifact`. What it is allowed to write is governed by the hook's `traits.produces` declaration in [`kit.yaml`](../kit-authoring/overview.md). For trait semantics, see [Concepts / Payload and traits](../guide/concepts.md#payload-and-traits).

### stderr (logs)

Whatever the hook writes to stderr is stored as job log output and surfaced by `boid job show <job-id>`. Use it freely for debug information; it does not affect the payload patch.

## Exit codes

| Exit code | Effect |
|---|---|
| `0` | Success. The payload patch (if any) is merged. |
| Non-zero | The job is marked `failed`. The task is **not** automatically aborted — the state machine's auto-transitions decide what happens next. |

Even on a non-zero exit, if `payload_patch.json` was written it is still merged.

## Extra context for agent hooks

A hook declared with `kind: agent` participates in instruction routing. Its TaskJSON has the `instructions` field populated with a map of `Instruction` values addressed to that hook. The claude-code kit's hook, for example, reads `instructions.main` and feeds it to the agent as the message.

The fields of `Instruction` are listed in [`project.yaml` reference / Instruction](project-yaml.md#instruction).

## Minimal example (Bash)

```bash
#!/usr/bin/env bash
set -euo pipefail

# Read input
TASK_JSON=$(cat)
TASK_ID=$(echo "$TASK_JSON" | jq -r .id)
echo "[my-hook] processing task $TASK_ID" >&2

# Do something — here, write a fixed value to artifact
mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<'JSON'
{
  "payload_patch": {
    "artifact": { "hello": "world" }
  }
}
JSON
```

## Related documents

- [Kit authoring overview](../kit-authoring/overview.md) — full kit-author guide.
- [`project.yaml` reference](project-yaml.md) — type definitions for `Instruction` etc.
- [Concepts / Payload and traits](../guide/concepts.md#payload-and-traits) — what the traits mean.
- [State machine](../guide/state-machine.md) — when hooks fire.
