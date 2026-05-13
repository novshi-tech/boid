# Handler script protocol reference

The complete I/O contract between `boid` and its hook / gate scripts (collectively, *handlers*).

The [Kit authoring overview](../kit-authoring/overview.md) summarises the protocol; this page is the canonical reference for inputs (stdin, environment, working directory), outputs (`payload_patch.json`, stdout, stderr), exit codes, and the data structures involved.

## Two handler kinds

There are two kinds of handler. They share the same I/O protocol; only the execution context differs.

| Kind | When it fires | Where it runs | Concurrency | Working directory |
|---|---|---|---|---|
| **Hook** | While the task is in a particular status (e.g. `executing`) | Inside the sandbox | If multiple hooks bind to the same status, they run in parallel | The worktree (or the project root) |
| **Gate** | At a state transition (entry / exit) | On the host (outside the sandbox) | Multiple gates on the same transition run in parallel | The worktree (or the project root) |

For how each is declared, see [`kit.yaml`](../kit-authoring/overview.md#main-kityaml-fields). The entry / exit `phase` is gate-specific and does not apply to hooks.

## Inputs

### stdin

When a handler is launched, stdin carries the entire task as a single JSON document (TaskJSON). The size is variable, so read until EOF before parsing.

The main fields of TaskJSON:

| Key | Type | Role |
|---|---|---|
| `id` | string | Task ID (UUID). |
| `project_id` | string | The owning project's ID. |
| `title` | string | Task title. |
| `description` | string | Free-form body. |
| `status` | string | Current state (`pending` / `executing` / ...). |
| `behavior` | string | Canonical behavior name (`supervisor` or `executor`); legacy aliases (`plan`, `dev`) are normalised before reaching handlers. |
| `traits` | list of string | Traits the behavior declared. |
| `readonly` | bool | Whether the sandbox is read-only (derived: `true` for supervisor, `false` for executor). |
| `worktree` | bool | Whether this task has a worktree (project-top `worktree:` flag combined with the behavior name). |
| `branch_prefix` | string | Worktree branch-name prefix (always `boid/` — no longer user-configurable). |
| `base_branch` | string | Worktree base branch (resolved from project-top `base_branch:` with `${TASK_REMOTE_ID}` / `${current_branch}` expansion). |
| `payload` | object | The full current payload — most handlers read from here. |
| `instructions` | map (role → Instruction) | Routed instructions; meaningful only to hooks declared `kind: agent`. |
| `auto_start` | bool | Whether the task was created with auto-start. |
| `depends_on` | list of string | Dependency task IDs. |
| `parent_id` | string | Optional parent task ID. |
| `created_at` / `updated_at` | RFC3339 timestamp | Creation / update times. |

The complete shape lives in the `Task` type at [`internal/orchestrator/model.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/model.go).

### Environment variables

The handler runs with the following environment variables set:

| Variable | Role |
|---|---|
| `BOID_TASK_ID` | Current task ID (same value as TaskJSON's `id`). |
| `BOID_JOB_ID` | Current job ID (used by `boid job show <id>`). |
| `BOID_PROJECT_ID` | Project ID. |
| `HOME` | The sandbox home for hooks; the host home for gates. |
| `PATH` | Inherited from the launcher; may be overridden by kit/behavior `env`. |

Any variables declared in the kit's `kit.yaml` or the behavior's `env` field are also exported.

### Working directory

- When the task has a worktree (project-top `worktree: true` combined with the executor behavior), the handler runs with the cwd set to **the worktree root**.
- Otherwise (supervisor task, or executor in a project without `worktree:`), the cwd is **the project root** (the directory containing `project.yaml`).

This means commands like `git`, `gh`, and language toolchains do not need explicit directory arguments.

### File system access

- **Hooks (inside the sandbox)** can read and write only inside the worktree (and nowhere if `readonly: true`). Paths declared in the kit's `additional_bindings` are mounted in addition. The host's home directory, SSH keys, and other projects are not visible.
- **Gates (on the host)** run with the user's normal host privileges. They are not sandboxed, which is why environment-specific operations such as `systemctl restart` belong here.

## Outputs

To update the payload, a handler returns a **payload patch**. Two output paths are supported, with a defined priority.

### Path 1: `$HOME/.boid/output/payload_patch.json` (preferred)

Write the JSON document to `$HOME/.boid/output/payload_patch.json`. When the handler exits, the runtime (sandbox or host wrapper) reads this file and forwards it to `boid`.

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

For new handlers, prefer the file path. Agent-style hooks (such as `claude-code`) print incidental output on stdout, so using the file path avoids accidental misparses.

### payload patch shape

The top level must be a `payload_patch` key. Its body is deep-merged into the current payload.

```json
{
  "payload_patch": {
    "artifact": {
      "<key>": "<value>"
    },
    "verification": {
      "findings": [
        {
          "status": "open",
          "severity": "error",
          "message": "..."
        }
      ]
    }
  }
}
```

Which traits a handler is allowed to write is governed by the handler's `traits.produces` declaration in [`kit.yaml`](../kit-authoring/overview.md). For trait semantics, see [Concepts / Payload and traits](../guide/concepts.md#payload-and-traits).

### stderr (logs)

Whatever the handler writes to stderr is stored as job log output and surfaced by `boid job show <job-id>`. Use it freely for debug information; it does not affect the payload patch.

## Exit codes

| Exit code | Effect |
|---|---|
| `0` | Success. The payload patch (if any) is merged. |
| Non-zero | The job is marked `failed`. The task is **not** automatically aborted — the state machine's auto-transitions decide what happens next. |

Even on a non-zero exit, if `payload_patch.json` was written it is still merged. This lets a failing job leave findings behind for the next state to pick up.

## Extra context for hooks

A hook declared with `kind: agent` participates in instruction routing. Its TaskJSON has the `instructions` field populated with a map of `Instruction` values addressed to that hook. The claude-code kit's hook, for example, reads `instructions.main` and feeds it to the agent as the message.

The fields of `Instruction` are listed in [`project.yaml` reference / Instruction](project-yaml.md#instruction).

## Extra context for gates

Gates run on the host. Because they bypass the sandbox:

- `kind:` is not allowed (gates do not participate in instruction routing).
- `host_commands` is irrelevant (the gate runs on the host and can call any command).
- The cwd is the worktree (or the project root) on the host.

The `phase` is either `entry` (fires just before the task enters the state) or `exit` (fires just before it leaves). Defaults to `exit` when omitted.

## Minimal examples

### Hook (Bash)

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

### Gate (Python)

```python
#!/usr/bin/env python3
import json
import os
import sys

task = json.load(sys.stdin)
print(f"[my-gate] task={task['id']} status={task['status']}", file=sys.stderr)

output_dir = os.path.join(os.environ["HOME"], ".boid", "output")
os.makedirs(output_dir, exist_ok=True)
with open(os.path.join(output_dir, "payload_patch.json"), "w") as f:
    json.dump({
        "payload_patch": {
            "verification": {
                "findings": [
                    {"status": "resolved", "message": "all checks pass"}
                ]
            }
        }
    }, f)
```

## Related documents

- [Kit authoring overview](../kit-authoring/overview.md) — full kit-author guide.
- [`project.yaml` reference](project-yaml.md) — type definitions for `Instruction` etc.
- [Concepts / Payload and traits](../guide/concepts.md#payload-and-traits) — what the traits mean.
- [State machine](../guide/state-machine.md) — when handlers fire.
