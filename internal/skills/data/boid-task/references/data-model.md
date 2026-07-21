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
| readonly | Whether this task is **read-only**. `true` → this job's `git push` is rejected by the git gateway (fetch still works); the sandbox clone itself is still a completely normal read-write filesystem regardless — nothing on disk is actually locked down. `false` → a normal writable task. This is the primary mode-determination signal (see the skill's Step 0) and the **only** authoritative source for it — do not infer writability from `pwd` or file permissions inside the clone. |

This re-derives live from the task row on every call (unlike a file frozen at dispatch time), so it reflects concurrent `boid task update` calls made by another session.

## boid task instructions

Array scoped to **this job** — in practice **zero or one element**, never a
growing history. It is the one entry from the task's instruction history that
was routed to this specific job (matched by invoked agent at plan time,
`internal/dispatcher/job_context.go`'s `routedInstructionSlice`), not the
task's full instruction history. On reopen, the new instruction becomes the
new job's sole element here — it does not get appended alongside earlier
ones, and earlier instructions are simply not present in this command's
output for the new job.

```yaml
- role: executor
  type: execution
  agent: claude-code
  message: "Fix the lint errors and re-push."
```

| Field | Description |
|-----------|------|
| role | Logical name of the instruction |
| type | `execution` only |
| agent | Target agent name |
| message | Specific instruction content |

Treat the single element as the active instruction. If you need context from
before this job started (e.g. what a prior turn was originally asked to do),
this command cannot give it to you — read `boid task current --field description`
or check `boid task payload` for artifacts a prior turn recorded.

## boid task payload

**Read-only.** Use it to read context such as artifacts accumulated by past hooks. This is not a path for agents to write to — use `boid task update --payload-patch @-` (see the skill's *Writing the final report* section).

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
「タスクコンテキストの伝搬」): the project directory *path* — the filesystem
layout is "見たまんま" (what you see is what you get) for that: the sandbox
clones the project fresh into your cwd for every job, so `pwd` tells you
where you are. **Writability is different: do not infer it from `pwd` or file
permissions.** The clone is always a completely normal read-write filesystem
at the OS level, whether the task is `readonly: true` or `false` — the actual
constraint is enforced at `git push` time by the git gateway, not by the
local filesystem (fetch always works; push is rejected for read-only tasks).
`boid task current --field readonly` is the only authoritative source for
whether this job may push — see that command's `readonly` field above. The
active-run harness name and the list of generally-available commands are
likewise not part of this schema — they are either fixed per adapter (not
per-job data worth a round trip) or discoverable by simply trying the
command. Workspace peer projects (other projects in the same workspace,
advertised for cross-project fetch/clone) are also not part of this
command's schema yet — that is a known open item in the Phase 5b plan, not
something this command currently exposes.
