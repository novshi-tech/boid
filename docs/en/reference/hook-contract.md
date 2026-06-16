# Hook script protocol reference

The complete I/O contract between `boid` and hook scripts.

The [Kit authoring overview](../kit-authoring/overview.md) summarises the protocol; this page is the canonical reference for inputs (stdin, environment, working directory), outputs (`payload_patch.json`, stdout, stderr), exit codes, and the data structures involved.

## Inputs

### stdin

All hook jobs run with `Interactive: true`, which means stdin is a PTY — **no data is written to stdin** at launch. Do not attempt to read TaskJSON from stdin; doing so will block indefinitely.

Task metadata is provided via **context files** written to `$HOME/.boid/context/` before the hook starts:

| File | Format | Contents |
|---|---|---|
| `task.yaml` | YAML | Core task fields (see table below). |
| `instructions.yaml` | YAML | Routing instructions (for `kind: agent` hooks). |
| `environment.yaml` | YAML | Additional environment metadata. |
| `payload.json` | JSON | The current full payload. |

**`task.yaml` fields** (the only fields present; the document is intentionally minimal):

| Key | Type | Role |
|---|---|---|
| `id` | string | Task ID (UUID). |
| `title` | string | Task title. |
| `status` | string | Current state (`pending` / `executing` / `awaiting` / `done` / `aborted`). |
| `behavior` | string | Behavior name (`supervisor` or `executor`). |
| `description` | string | Free-form body. |

Read the context files at script startup before doing any other work:

```bash
TASK_ID=$(yq -r .id "$HOME/.boid/context/task.yaml")
PAYLOAD=$(cat "$HOME/.boid/context/payload.json")
```

> **Non-interactive jobs only**: hooks that set `kind: exec` (non-interactive) receive a trait-filtered payload on stdin in addition to the context files. Interactive agent hooks do not receive stdin data.

The complete task shape lives in the `Task` type at [`internal/orchestrator/spec_types.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/spec_types.go).

### Environment variables

The hook runs with the following environment variables set:

| Variable | Role |
|---|---|
| `BOID_TASK_ID` | Current task ID. |
| `BOID_JOB_ID` | Current job ID (used by `boid job show <id>`). |
| `BOID_BASE_BRANCH` | The task's `base_branch` (the PR target branch). Set for both root and child tasks. |
| `BOID_PARENT_BRANCH` | The parent task's HEAD branch. Empty for root tasks. Used by sub-supervisors (e.g. `git merge $BOID_PARENT_BRANCH`). |
| `BOID_MODEL` | The model name configured for this task's instruction. |
| `BOID_INVOKED_ROLE` | The role name that triggered this hook invocation. |
| `BOID_INVOKED_NAME` | The hook name within the role. |
| `BOID_INVOKED_BEHAVIOR` | The behavior name (`supervisor` or `executor`). |
| `BOID_INSTRUCTIONS` | Serialized instructions passed to this hook (for `kind: agent` hooks). |
| `BOID_INTERACTIVE` | `1` if the job is interactive (PTY), `0` otherwise. |
| `BOID_BUILTIN_SHIM` | Path to the built-in shim binary injected into the sandbox. |
| `BOID_HOST_IP` | IP address of the host, reachable from inside the sandbox. |
| `BOID_BROKER_SOCKET` | Path to the host-command broker UNIX socket. |
| `BOID_BROKER_TOKEN` | Auth token for the broker socket. |
| `BOID_SOCKET` | Path to the boid daemon UNIX socket (for `boid` CLI calls from inside the hook). |
| `BOID_AGENT_SESSION_ID` | Session ID of the running agent job. Used by Q&A resume flows to correlate the answer with the correct agent session. |
| `BOID_USER_ANSWER` | The user's answer text, populated when the hook is resumed after a `notify --ask` question. |
| `BOID_QUESTION_ID` | The question ID corresponding to `BOID_USER_ANSWER`. |
| `TERM` | Terminal type (e.g. `xterm-256color`). |
| `HOME` | The sandbox home directory. |
| `PATH` | Inherited from the launcher; may be overridden by the kit's `env`. |

> **Note**: `BOID_PROJECT_ID` is **not** set in the hook environment. It is only exported by the `boid task notify` command internally.

> **Q&A resume** (`BOID_AGENT_SESSION_ID` / `BOID_USER_ANSWER` / `BOID_QUESTION_ID`): when an agent hook calls `boid task notify --ask`, boid suspends the task. When the user answers, boid resumes the hook with these three variables populated. Kit authors can use them to branch on the answer or route it back to the agent.

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

A hook declared with `kind: agent` participates in instruction routing. The routed instructions are available via `$HOME/.boid/context/instructions.yaml` and the `BOID_INSTRUCTIONS` environment variable. The claude-code kit's hook, for example, reads `instructions.main` from the context file and feeds it to the agent as the message.

The fields of `Instruction` are listed in [`project.yaml` reference / Instruction](project-yaml.md#instruction).

## Minimal example (Bash)

```bash
#!/usr/bin/env bash
set -euo pipefail

# Read task metadata from context files (stdin is a PTY — do not read from it)
TASK_ID=$(yq -r .id "$HOME/.boid/context/task.yaml")
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
