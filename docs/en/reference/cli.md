# CLI reference

A complete index of `boid`'s subcommands, grouped by what they do. The authoritative source for individual flag detail is `boid <subcommand> --help`. Use this page as the one-screen overview of "what is available".

## Common

### Invocation

`boid` with no arguments launches the TUI.

```bash
boid                        # launch the TUI
boid --help                 # list subcommands
boid <command> --help       # per-command help
```

### Global flags

| Flag | Purpose |
|---|---|
| `-o, --output {plain,json,yaml}` | Output format (default `plain`). `json` is convenient for scripting. |

### Auto-start

If the daemon is not running when you invoke any other command, `boid` starts it automatically (the exceptions are `start`, `stop`, and `gc`). You rarely need to call `boid start` by hand.

## Server lifecycle

| Command | Role |
|---|---|
| `boid start [--http-addr ADDR] [--db-path PATH] [--socket-path PATH] [--kits-dir DIR] [--key-file-path PATH]` | Start the daemon (it forks itself into a detached child and returns immediately). |
| `boid stop` | Stop the daemon. Killing by PID can leave a stale socket; prefer this. |
| `boid gc [--older-than DURATION]` | Garbage collect old completed/aborted tasks (the daemon also runs this on its own at startup). |
| `boid check` | Check host prerequisites and hook dependencies. |
| `boid init [DIR]` | Interactively scaffold a new project. |

See [Getting started / Install](../getting-started/01-install.md) for context.

## Project

Manage projects ([`project.yaml` reference](project-yaml.md)).

| Command | Role |
|---|---|
| `boid project add <dir>` | Register `<dir>/.boid/project.yaml` with `boid`. |
| `boid project list` | List registered projects. |
| `boid project show <ref>` | Show details (id exact-match or name partial-match). |
| `boid project remove <ref>` | Unregister a project. |
| `boid project reload` | Re-read every registered project's `project.yaml`. |
| `boid project behaviors <ref>` | List `task_behaviors` defined in the project. |

### `project local` (editing `.boid/project.local.yaml`)

Local-only overrides (intended to be `gitignore`d). Lets you add `host_commands` / `additional_bindings` / `env` without sharing them with the team.

| Command | Role |
|---|---|
| `boid project local init [DIR]` | Create an empty `project.local.yaml`. |
| `boid project local show [DIR]` | Print the file. |
| `boid project local set-env <key> <value> [DIR]` | Add an env override. |
| `boid project local unset-env <key> [DIR]` | Remove an env override. |
| `boid project local add-binding <path> [DIR]` | Add an additional binding. |
| `boid project local remove-binding <path> [DIR]` | Remove an additional binding. |

## Task

Creating, observing, and updating tasks lives under `boid task`. See [Concepts / Task](../guide/concepts.md#task) and [State machine](../guide/state-machine.md) for the model.

| Command | Role |
|---|---|
| `boid task list [--status STATUS] [--workspace ID] [--behavior NAME] [--has-depends-on \| --no-depends-on]` | List tasks. |
| `boid task create [-f FILE]` | Create a task; YAML on stdin (or via `-f`). |
| `boid task show <id>` | Task detail (status and payload). |
| `boid task watch <id> [--interval DURATION]` | Stream status / payload changes live. |
| `boid task get <id> --field <name>` | Fetch a single field (e.g. `--field title`). |
| `boid task update <id> [--patch-file FILE] [--payload-file FILE] [--instructions-file FILE]` | Update a task; use `-` for stdin. |
| `boid task delete <id> [--force]` | Delete a task (`--force` if active). |
| `boid task duplicate <source_id> [--auto-start]` | Duplicate an existing task. |
| `boid task reopen <id> [--message MSG]` | Return a `done` task to `executing`, appending the `--message` text as a new entry on `Task.Instructions` (e.g. when auto-merge hits a conflict). |
| `boid task rerun <id> [--auto-start] [--instructions-file FILE]` | Reset a `done` / `aborted` task to `pending` and re-run it under the same ID. |
| `boid task notify <id> --message MSG` | Send a notification to the user from an agent. Invokes `notify.command` from `~/.config/boid/config.yaml`. Used in `boid-plan` supervisor mode to request user approval or escalate when a hard cap is reached. |
| `boid task import [-f FILE] [--project ID] [--datasource ID]` | Bulk import tasks from JSONL. |

The notify script receives: `BOID_TASK_ID`, `BOID_TASK_TITLE`, `BOID_PROJECT_ID`, `BOID_PROJECT_NAME`, `BOID_MESSAGE`, `BOID_TASK_URL` (set only when `web.public_url` is configured).

### `task create` input

YAML schema:

```yaml
project_id: <id>
title: <string>
behavior: <name>            # or behavior_spec
auto_start: false
description: ...
payload:    { ... }
instructions: { ... }
depends_on:  [<task-id>, ...]
depends_on_payload: <expr>
```

Pass `behavior_spec` to specify the behavior inline instead of referencing a name in `project.yaml`'s `task_behaviors`.

### `task gate` (per-task gate operations)

| Command | Role |
|---|---|
| `boid task gate list <task-id>` | List gates that fire on the task's current status. |
| `boid task gate replay <task-id> <gate-id>` | Replay a specific gate. |

### Inspection helpers

| Command | Role |
|---|---|
| `boid task artifacts <id>` | Pretty-print `payload.artifact`. |
| `boid task tree [<id>]` | Show the parent/child task tree. |

## Action

Issue a manual transition on a task.

```bash
boid action send --task <task-id> --type <action-type> [--payload FILE]
```

Common `<action-type>` values: `start`, `done`, `reopen`, `abort`. See [State machine / Manual transitions](../guide/state-machine.md#manual-transitions). To reopen a task with a new instruction, prefer `boid task reopen <id> --message "..."`.

## Job

Inspect handler execution records.

| Command | Role |
|---|---|
| `boid job list --task <task-id>` | All jobs that ran for a task. |
| `boid job show <job-id>` | Job detail (status / exit_code / full output). |
| `boid job watch <job-id>` | Block until the job finishes. |
| `boid job log <job-id>` | Show the execution transcript. |
| `boid job done <job-id> [--exit-code N] [--output-file FILE]` | (Internal) Notify the daemon that a job finished. |

`boid job done` is normally invoked by the sandbox EXIT trap or the host-gate wrapper; you would not type it by hand.

## Kit

Install and update [extension packages](../kit-authoring/overview.md).

| Command | Role |
|---|---|
| `boid kit install [repo]` | `git clone` a repository into `~/.local/share/boid/kits/`. With no argument, install all repos referenced by the current project. |
| `boid kit list` | List installed repositories. |
| `boid kit update <repo>` | `git pull` an installed repository. |
| `boid kit remove <repo>` | Remove an installed repository. |

## Web

Manage [Web UI](../guide/web-ui.md) device authentication.

| Command | Role |
|---|---|
| `boid web pair` | Issue a pairing code (5-minute lifetime, single use). |
| `boid web devices` | List paired devices. |
| `boid web revoke <id>` | Revoke a specific device. |
| `boid web revoke-all` | Revoke every device. |
| `boid web set-url <URL>` | Write the public URL to `config.yaml` (used to render magic links). |

## Secret

Encrypted storage for tokens and similar values. The encryption key is `~/.local/share/boid/secret.key`.

| Command | Role |
|---|---|
| `boid secret set <key>` | Store a value (read from stdin or interactive prompt). |
| `boid secret get <key>` | Retrieve a value. |
| `boid secret list` | List keys. |
| `boid secret delete <key>` | Remove a value. |

## Workspace

Group several projects together.

| Command | Role |
|---|---|
| `boid workspace list` | List workspaces. |
| `boid workspace show <id>` | Show projects and recent tasks in a workspace. |
| `boid workspace assign <project-ref> <workspace-id>` | Assign a project to a workspace. |
| `boid workspace clear <project-ref>` | Remove a project's workspace assignment. |

## Sandbox operations

| Command | Role |
|---|---|
| `boid exec -p <project-ref> [command-name]` | Run a named command (declared in the project's `commands`) inside the project sandbox. |
| `boid attach <job-id>` | Attach to a running job's runtime (for interactive jobs). |

## Output formats

Almost every command supports `-o json` for downstream tooling such as `jq`.

```bash
boid task list -o json | jq '.[] | select(.status=="executing")'
boid task show <id> -o yaml
```

## Related documents

- [Getting started](../getting-started/) — guided tutorials.
- [Concepts](../guide/concepts.md) — meanings of task / job / hook / gate / kit / payload / trait.
- [State machine](../guide/state-machine.md) — manual and automatic transition rules.
- [`project.yaml` reference](project-yaml.md) — project-definition fields.
- [Handler script protocol](handler-contract.md) — hook / gate I/O contract.
