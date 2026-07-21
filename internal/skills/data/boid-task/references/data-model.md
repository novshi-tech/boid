# Data Model

Task context is fetched live via four `boid task ...` broker RPCs (Phase 5b,
docs/plans/phase5-shim-and-task-context.md) rather than read from files. Each
command prints YAML by default; add `--format json` for JSON, or
`--field <dotted.path>` for a single value (e.g. `boid task current --field title`).

## Contents

- [boid task current](#boid-task-current)
- [boid task instructions](#boid-task-instructions)
- [boid task payload](#boid-task-payload)
- [boid task env](#boid-task-env)

## boid task current

```yaml
id: "abc-12345678"
title: "Implement user authentication"
description: "Add a login feature using OAuth2"
status: "executing"
behavior: "executor"
readonly: false
```

Canonical behavior names are `supervisor` (readonly orchestrator) and `executor` (writable implementer). The legacy keys `plan` / `dev` are accepted as aliases and rewritten by the daemon, so by the time you read this the field is usually already canonical.

| Field | Description |
|-----------|------|
| id | Unique task identifier |
| title | Task title |
| description | Detailed task description |
| status | Current state — one of `pending`, `executing`, `awaiting`, `done`, `aborted` |
| behavior | Task execution model name |
| readonly | Whether this task's project directory is writable. The primary mode-determination signal — see the skill's Step 0. |

This re-derives live from the task row on every call (unlike a file frozen at dispatch time), so it reflects concurrent `boid task update` calls made by another session.

## boid task instructions

Array of instructions addressed to you, scoped to **this job**. The last element is the current active instruction; new instructions are appended each time the task is reopened. Past instructions remain at the front of the array so you can trace what was requested before.

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

## boid task payload

**Read-only.** Use it to read context such as artifacts accumulated by past hooks. This is not a path for agents to write to — use `boid task update --payload-file` (see the skill's *Writing the final report* section).

Instructions are not a trait of the payload; they are delivered by the separate `boid task instructions` command.

## boid task env

Dynamic constraint information for the sandbox — deliberately reduced to the
two properties an in-sandbox agent cannot observe on its own. Everything a
legacy pre-cutover environment description used to cover beyond these two —
sandbox implementation details, filesystem layout, the active harness name,
and the list of available tools — is either hard-coded scenery or directly
observable from inside the container: `pwd`, file permissions, and trying the
command and reading the error tell you the rest.

```yaml
allowed_domains:
  - github.com
  - api.anthropic.com
host_commands:
  - name: gh
    allow: ["pr", "issue", "repo"]
    deny: ["auth"]
    reject:
      - match: "gh auth *"
        reason: "credentials are host-managed; use the gh command as-is without auth subcommands"
```

| Field | Description |
|-----------|------|
| allowed_domains | Network egress allowlist. Requests outside this list are blocked by the sandbox proxy. |
| host_commands | Commands that dispatch to the host broker instead of running inside the sandbox (e.g. `gh`), with their argument policy. |
| host_commands[].name | The command's short name (what you type on the shell). |
| host_commands[].allow | Allowed leading argument patterns, if the command restricts arguments. |
| host_commands[].deny | Denied leading argument patterns. |
| host_commands[].reject | `{match, reason}` rules: a call matching `match` is rejected with `reason` explaining what to do instead. |

**Not included** (post-cutover container model, `docs/plans/container-based-boid.md`
「タスクコンテキストの伝搬」): the project directory path and its writability —
the filesystem is "見たまんま" (what you see is what you get): the sandbox clones
the project fresh into your cwd for every job, so `pwd` and normal file
permission checks tell you everything you need there. The active-run harness
name and the list of generally-available commands are likewise not part of
this schema — they are either fixed per adapter (not per-job data worth a
round trip) or discoverable by simply trying the command. Workspace peer
projects (other projects in the same workspace, advertised for cross-project
fetch/clone) are also not part of this command's schema yet — that is a known
open item in the Phase 5b plan, not something this command currently exposes.
