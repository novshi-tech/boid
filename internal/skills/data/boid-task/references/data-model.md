# Data Model

## Contents

- [task.yaml](#taskyaml)
- [instructions.yaml](#instructionsyaml)
- [payload.yaml](#payloadyaml)
- [environment.yaml](#environmentyaml)

## task.yaml

```yaml
id: "abc-12345678"
title: "Implement user authentication"
description: "Add a login feature using OAuth2"
status: "executing"
behavior: "executor"
```

Canonical behavior names are `supervisor` (readonly orchestrator) and `executor` (writable implementer). The legacy keys `plan` / `dev` are accepted as aliases and rewritten by the daemon, so by the time you read `task.yaml` the field is usually already canonical.

| Field | Description |
|-----------|------|
| id | Unique task identifier |
| title | Task title |
| description | Detailed task description |
| status | Current state — one of `pending`, `executing`, `awaiting`, `done`, `aborted` |
| behavior | Task execution model name |

## instructions.yaml

Array of instructions addressed to you. The last element is the current active instruction; new instructions are appended each time the task is reopened. Past instructions remain at the front of the array so you can trace what was requested before.

```yaml
- role: executor
  type: execution
  agent: claude-code
  message: "Implement using TDD. Write tests first."
- role: executor
  type: execution
  agent: claude-code
  message: "Fix the lint errors and re-push."   # appended on reopen
```

| Field | Description |
|-----------|------|
| role | Logical name of the instruction |
| type | `execution` only |
| agent | Target agent name |
| message | Specific instruction content |

Read the last element as the primary instruction, and refer to earlier elements as context when needed.

## payload.yaml

**Read-only** input file. Use it to read context such as artifacts accumulated by past hooks. This is not a path for agents to write to.

Instructions are not a trait of the payload; they are delivered as a separate file (`instructions.yaml`) alongside `task.yaml`.

## environment.yaml

Dynamic constraint information for the sandbox.

```yaml
readonly: false
worktree: false
network:
  restricted: true
tools:
  - git
  - python3
workspace_projects:
  - name: shared-lib
    clone_url: http://10.0.2.2:9001/j/<token>/github.com/owner/shared-lib.git
    reference_path: /mnt/refs/peers/<peer-project-id>.git
```

| Field | Description |
|-----------|------|
| readonly | Whether the project directory is writable |
| worktree | Legacy field, always `false` post-cutover. Every job now sees a fresh sandbox-internal clone of the project (see `filesystem.project_dir`), not a shared host git worktree — commits are only visible to other sessions/hosts once pushed. `readonly: true` no longer means the filesystem is read-only; it means `git push` is rejected by the git gateway (fetch still works). |
| network.restricted | Whether external network access is restricted |
| tools | Available commands |
| workspace_projects | Other projects in the same workspace (fetch-only from this job's perspective). Post-cutover each entry advertises the **name** (repo basename), the **clone_url** through the git gateway (fetch-only for peers — writing to a peer means creating a cross-project child task instead), and the **reference_path** — a read-only bind-mounted `.git` directory usable as `git clone --reference <reference_path> <clone_url>` for object-sharing acceleration. There is **no legacy `path:` field** — peers are no longer exposed as a live host filesystem path; if you need to see a peer's working tree, `git clone` it. Peers whose upstream_url could not be resolved are silently omitted from the list. |
