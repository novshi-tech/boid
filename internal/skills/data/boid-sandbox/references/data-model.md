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
behavior: "dev"
```

| Field | Description |
|-----------|------|
| id | Unique task identifier |
| title | Task title |
| description | Detailed task description |
| status | Current state (see [state-machine.md](state-machine.md)) |
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
  - path: /home/user/shared-lib
    name: shared-lib
```

| Field | Description |
|-----------|------|
| readonly | Whether the project directory is writable |
| worktree | Whether running in git worktree mode |
| network.restricted | Whether external network access is restricted |
| tools | Available commands |
| workspace_projects | Other projects in the same workspace (read-only) |
