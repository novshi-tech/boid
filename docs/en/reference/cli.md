# CLI reference

A complete index of `boid`'s subcommands, grouped by what they do. The authoritative source for individual flag detail is `boid <subcommand> --help`. Use this page as the one-screen overview of "what is available".

## Common

### Invocation

`boid` with no arguments prints help.

```bash
boid --help                 # list subcommands
boid <command> --help       # per-command help
```

### Global flags

| Flag | Purpose |
|---|---|
| `-o, --output {plain,json,yaml}` | Output format (default `plain`). `json` is convenient for scripting. |

### Auto-start

If the daemon is not running when you invoke any other command, `boid` starts it automatically. The exceptions (commands that skip auto-start) are: `start`, `stop`, `gc`, `check`, `init`, `fetch`, `web set-url`, `web set-addr`, and `project migrate`. You rarely need to call `boid start` by hand.

Set `BOID_NO_AUTOSTART=1` to disable auto-start globally.

### Command scope classification (remote / local / neutral)

Every command is internally classified as `remote` (works purely through the daemon's HTTP API, including a future remote daemon), `local` (depends on daemon lifecycle, or on the filesystem of the host the CLI process itself runs on), or `neutral` (needs no daemon connection at all) — the `boid.scope` cobra annotation, required on every leaf command (an unclassified command fails the build). This classification does not yet gate anything at runtime; it was introduced in Phase 2.5 as groundwork for Phase 3 (CLI remote connection). See `docs/plans/cli-remote-connection.md` for details.

## Server lifecycle

| Command | Role |
|---|---|
| `boid start [--db-path PATH] [--socket-path PATH] [--kits-dir DIR] [--key-file-path PATH]` | Start the daemon (it forks itself into a detached child and returns immediately). HTTP address is configured via `web.http_addr` in `config.yaml` or `boid web set-addr`. |
| `boid stop` | Stop the daemon. Killing by PID can leave a stale socket; prefer this. |
| `boid gc [--older-than DURATION] [--dry-run]` | Garbage collect old completed/aborted tasks (the daemon also runs this on its own at startup). `--dry-run` prints what would be deleted without removing anything. Output also lists every workspace home's on-disk size (display only, never deletes — see the [workspace home guide](../guide/workspace-home.md#boid-gcs-workspace-home-listing)). |
| `boid check` | Check host prerequisites and hook dependencies. |
| `boid init [DIR]` | **(Deprecated)** Prints a deprecation guide. Use `boid project init\|add` (plus, optionally, `boid workspace create/edit/import`) instead. See [Onboarding](../guide/onboarding.md). |

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

### `project local` — Deprecated

`boid project local ...` commands have been removed.
The `host_commands` / `env` / `secret_namespace` overrides that `project.local.yaml` used to provide are now part of the workspace (DB-backed, machine-local). `additional_bindings` was retired in Phase 4 PR4 — persist a toolchain via the [workspace home `init.sh`](../guide/workspace-home.md) instead.
Run `boid project migrate <dir>` to convert automatically. See the [Migration guide](../guide/migration.md).

## Task

Creating, observing, and updating tasks lives under `boid task`. See [Concepts / Task](../guide/concepts.md#task) and [State machine](../guide/state-machine.md) for the model.

| Command | Role |
|---|---|
| `boid task list [--status STATUS] [--workspace ID] [--behavior NAME]` | List tasks. |
| `boid task create [-f FILE]` | Create a task; YAML on stdin (or via `-f`). |
| `boid task show <id> [--field PATH]` | Task detail (status and payload). With `--field <path>`, prints a single value as plain text — dotted JSON path resolved against the task (e.g. `--field status`, `--field payload.artifact.report`, `--field awaiting.question`, `--field lifecycle.abort.message`). |
| `boid task watch <id> [--interval DURATION]` | Stream status / payload changes live. |
| `boid task update <id> [-f FILE \| --patch-file FILE] [--payload-file FILE] [--instructions-file FILE]` | Update a task; use `-` for stdin. `-f` is a shorthand for `--patch-file`. |
| `boid task delete <id> [--force]` | Delete a task (`--force` if active). |
| `boid task duplicate <source_id> [--auto-start]` | Duplicate an existing task. |
| `boid task reopen <id> [-m MSG \| --message MSG]` | Return a `done` task to `executing`, appending the `--message` text as a new entry on `Task.Instructions` (e.g. when auto-merge hits a conflict). `-m` is a shorthand for `--message`. |
| `boid task rerun <id> [--auto-start] [--instructions-file FILE]` | Reset a `done` / `aborted` task to `pending` and re-run it under the same ID. |
| `boid task notify <id> --message MSG [--ask QUESTION] [--question-id ID] [--done] [--fail] [--progress] [--session-id ID]` | Send a notification to the user from an agent. Invokes `notify.command` from `~/.config/boid/config.yaml`. With `--ask`, enters Q&A mode and transitions the task to `awaiting`. |
| `boid task answer --task ID --question-id ID --answer TEXT` | Submit a user reply to an `awaiting` task. Transitions the task `awaiting → executing` and restarts the hook. |
| `boid task import [-f FILE] [--project ID]` | Bulk import tasks from JSONL. |

The notify script receives: `BOID_TASK_ID`, `BOID_TASK_TITLE`, `BOID_PROJECT_ID`, `BOID_PROJECT_NAME`, `BOID_MESSAGE`, `BOID_TASK_URL` (set only when `web.public_url` is configured).

#### `boid task notify` flags

| Flag | Required | Description |
|---|---|---|
| `--message, -m MSG` | ◎ (except `--progress`) | Notification text. Passed to the notify script as `BOID_MESSAGE`. Required for all modes except `--progress`. |
| `--ask QUESTION` | | Question text. Transitions the task to `awaiting` (Q&A mode). |
| `--question-id ID` | | UUID identifying this Q&A turn. Auto-generated when omitted. |
| `--done` | | Signal successful completion. Records a `done_request` lifecycle entry; the daemon transitions the task to `done` after the job exits. |
| `--fail` | | Signal failure. Records a `fail_request` lifecycle entry; the daemon transitions the task to `aborted` after the job exits. |
| `--progress` | | Record a progress entry on the timeline only (no state change, `--message` optional). |
| `--session-id ID` | | Associate this notification with a specific agent session. |

`--ask`, `--done`, `--fail`, and `--progress` are mutually exclusive. Without any of them, this is a plain FYI notification (no state change).

```bash
# Plain notification
boid task notify ${BOID_TASK_ID} --message "Please review PR #42"

# Q&A mode (transitions to awaiting)
boid task notify ${BOID_TASK_ID} \
  --message "A merge decision is needed" \
  --ask "Should I merge PR #42?"

# Signal done (task transitions to done after job exits)
boid task notify ${BOID_TASK_ID} --done --message "All done"

# Signal failure (task transitions to aborted after job exits)
boid task notify ${BOID_TASK_ID} --fail --message "Encountered an error"

# Progress update (timeline only, no state change)
boid task notify ${BOID_TASK_ID} --progress --message "Step 2 of 5 complete"
```

#### `boid task answer` flags

| Flag | Required | Description |
|---|---|---|
| `--task ID` | ◎ | ID of the task to answer |
| `--question-id ID` | ◎ | UUID of the Q&A turn being answered |
| `--answer TEXT` | ◎ | Answer text |

**Exit codes**:
- `0`: Answer saved; task transitioned `awaiting → executing`.
- `1`: Task is not in `awaiting` state, or an argument is missing.

```bash
boid task answer \
  --task 550e8400-e29b-41d4-a716-446655440000 \
  --question-id q-abc-123 \
  --answer "yes"
```

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
```

Pass `behavior_spec` to specify the behavior inline instead of referencing a name in `project.yaml`'s `task_behaviors`.

### `task hook` (per-task hook operations)

| Command | Role |
|---|---|
| `boid task hook list <task-id> [--status STATUS]` | List hooks that fire on the task's current status. `--status` filters by hook job status. |
| `boid task hook replay <task-id> <hook-id> [--status STATUS]` | Replay a specific hook. `--status` filters by hook job status. |

If an agent hook was interrupted (e.g., by `boid stop`), use `boid task hook list <task-id>` to see which hooks can be re-fired, then `boid task hook replay <task-id> <hook-id>` to recover.

### Inspection helpers

| Command | Role |
|---|---|
| `boid task artifacts <id> [--field PATH] [--output-file FILE]` | Pretty-print `payload.artifact`. `--field` extracts a single field; `--output-file` writes the output to a file. |
| `boid task tree [<id>]` | Show the parent/child task tree. |

## Action

Issue a manual transition on a task.

```bash
boid action send --task <task-id> --type <action-type> [--payload FILE]
```

Common `<action-type>` values: `start`, `done`, `reopen`, `abort`. See [State machine / Manual transitions](../guide/state-machine.md#manual-transitions). To reopen a task with a new instruction, prefer `boid task reopen <id> --message "..."`.

## Job

Inspect hook execution records.

| Command | Role |
|---|---|
| `boid job list --task <task-id>` | All jobs that ran for a task. |
| `boid job show <job-id>` | Job detail (status / exit_code / full output). |
| `boid job watch <job-id> [--interval DURATION]` | Block until the job finishes. `--interval` sets the polling interval. |
| `boid job log <job-id>` | Show the execution transcript. |
| `boid job done <job-id> [--exit-code N] [--output-file FILE]` | (Internal) Notify the daemon that a job finished. |

`boid job done` is normally invoked by the sandbox EXIT trap; you would not type it by hand.

## Kit (removed as a command)

`boid kit init` / `boid kit list` / `boid kit remove`, and `boid workspace configure`, were removed in Phase 2.5 PR6 (2026-07). `env` is now set directly on a workspace via the [Workspace](#workspace) CLI (`additional_bindings` was retired in Phase 4 PR4 — use the [workspace home `init.sh`](../guide/workspace-home.md) instead). `host_commands` is different: a workspace only holds a `[]string` list of *reference names* (`host_commands: [gh, aws]`); the actual command definitions (`path` / `allow` / `deny` / `env`) live in the daemon-wide `~/.config/boid/host_commands.yaml`, managed separately — see [Host Commands](#host-commands) below (or [Onboarding / Defining host_commands](../guide/onboarding.md#defining-host_commands-the-daemon-wide-registry)) for how to populate it now that `kit init` no longer does it automatically.

The `kit.yaml` file format itself hasn't gone away (hand-writing one and pointing a workspace at it still works). Phase 2.5 PR7 removed the `WorkspaceMeta.Kits` field from the code outright, so passing `kits:` directly to `boid workspace create/edit/import` is now rejected. What's left is `boid workspace assign`'s auto-create convenience path (resolving a legacy shadow yaml's `kits:` client-side, once) and the legacy kit `boid project migrate` generates (its `host_commands` is folded directly into the workspace, no kit-directory round trip; any legacy `additional_bindings` is ignored per Phase 4 PR4's retirement). For the file format see [Kit authoring overview](../kit-authoring/overview.md); for the retirement's background see [Onboarding / On the retirement of the kit mechanism](../guide/onboarding.md#on-the-retirement-of-the-kit-mechanism).

## Web

Manage [Web UI](../guide/web-ui.md) device authentication.

| Command | Role |
|---|---|
| `boid web pair [--label LABEL]` | Issue a pairing code (5-minute lifetime, single use). `--label` sets a human-readable label for the new device. |
| `boid web devices` | List paired devices. |
| `boid web revoke <id>` | Revoke a specific device. |
| `boid web revoke-all` | Revoke every device. |
| `boid web set-url <URL>` | Write the public URL to `config.yaml` (used to render magic links). |
| `boid web set-addr <ADDR>` | Write the HTTP listen address to `config.yaml` (e.g. `boid web set-addr :9090`). Takes effect on the next daemon start. |

## Secret

Encrypted storage for tokens and similar values. The encryption key is `~/.local/share/boid/secret.key`.

| Command | Role |
|---|---|
| `boid secret set <key> [-n NAMESPACE \| --namespace NAMESPACE]` | Store a value (read from stdin or interactive prompt). |
| `boid secret get <key> [-n NAMESPACE \| --namespace NAMESPACE]` | Retrieve a value. |
| `boid secret list [-n NAMESPACE \| --namespace NAMESPACE]` | List keys. |
| `boid secret delete <key> [-n NAMESPACE \| --namespace NAMESPACE]` | Remove a value. |

## Workspace

Groups a project's runtime environment (`host_commands` / `env` / `capabilities` / `allowed_domains`) at the machine level. Backed by the `workspaces` table (Phase 2.5); the `default` workspace is always created automatically at daemon startup. Registering a project assigns it to `default` automatically, and `boid project init/add --workspace <slug>` is get-or-create (an unknown slug gets an empty workspace created for it before assignment). Every workspace has a persistent `$HOME` (workspace home); install a toolchain into it via [`init.sh`](../guide/workspace-home.md), not `additional_bindings` (retired).

| Command | Role |
|---|---|
| `boid workspace list` | List workspaces. |
| `boid workspace show <slug>` | Show a workspace's definition (host_commands/env/capabilities) and its assigned projects. |
| `boid workspace create <slug> [--from-file <yaml>]` | Create a new workspace (omit `--from-file` for a blank one). |
| `boid workspace edit <slug> --from-file <yaml>` | Replace an existing workspace wholesale (automatic If-Match; `--force` for last-write-wins). |
| `boid workspace import <file> [--mode create-only\|replace] [--slug SLUG]` | Import a workspace definition from yaml. `--mode` defaults to `create-only` (409 on an existing slug). |
| `boid workspace export <slug> [--output FILE]` | Export a workspace's definition as yaml (stdout by default). |
| `boid workspace assign <project-ref> <workspace-id>` | Assign a project to a workspace (404s on an unknown slug, unless a local `workspace.yaml` for it exists — auto-created from that). |
| `boid workspace clear <project-ref>` | Reset a project's workspace assignment to `default`. |
| `boid workspace remove <slug> [--force\|--yes]` | Remove a workspace (assigned projects are re-assigned to `default`; `default` itself cannot be removed). Prompts for confirmation, showing the home directory's size (`--force`/`--yes` skips it) — see the [workspace home guide](../guide/workspace-home.md#removing-a-workspace). |

## Host Commands

Inspect and reload the daemon's aggregated `~/.config/boid/host_commands.yaml` (the preflight-aggregated `host_commands` across all workspaces, Phase 2.5 PR4).

| Command | Role |
|---|---|
| `boid host-commands list` | List the host_commands names known to the daemon. |
| `boid host-commands reload` | Tell the daemon to re-read `host_commands.yaml` after a hand edit. |

## Sandbox operations

| Command | Role |
|---|---|
| `boid exec -p <project-ref> [--name NAME] [--readonly] -- <argv...>` | Run an arbitrary argv inside the project sandbox. Inherits the project's `host_commands` / `env` (`additional_bindings` was retired in Phase 4 PR4; toolchain lives in the workspace home). Everything after `--` is the in-sandbox argv (the legacy named-command form was retired in Phase 3-d). `--name` sets a display name; `--readonly` mounts the workspace read-only. |
| `boid attach <job-id>` | Attach to a running job's runtime (for interactive jobs). |
| `boid fetch <url>` | Fetch and print the content of a URL from the host (usable inside a sandbox where direct HTTP access may be restricted). |

## Agent

Control running agent jobs.

| Command | Role |
|---|---|
| `boid agent claude   -p <project> [--resume <session-id>] [--instruction "..."] [--readonly] [--model M] [--name NAME] [--no-attach]` | Start a claude session inside the project sandbox and attach to its PTY. `--resume` resumes an existing session; `--no-attach` prints the job id and exits. |
| `boid agent codex    -p <project> [same flags]` | **[Experimental]** Start a codex session. Launches the `codex` TUI inside the sandbox when no `--instruction` is given; with `--instruction` falls through to `codex exec` (one-shot smoke). Session persistence, `boid task notify` integration, and usage accounting are not yet implemented (see `docs/plans/multi-harness-production.md`). |
| `boid agent opencode -p <project> [same flags]` | **[Experimental]** Start an opencode session. Launches the `opencode <project>` TUI inside the sandbox when no `--instruction` is given; with `--instruction` falls through to `opencode run` (one-shot smoke). Session persistence, `boid task notify` integration, and usage accounting are not yet implemented (see `docs/plans/multi-harness-production.md`). |
| `boid agent stop <job-id>` | Send SIGUSR1 to the agent process, requesting a graceful stop. |

To open an interactive shell inside a project sandbox, use `boid exec -p <project> -- bash`. The former `boid agent shell` variant was retired after the git gateway cutover.

## Shell completion

```bash
boid completion bash   # generate Bash completion script
boid completion zsh    # generate Zsh completion script
boid completion fish   # generate Fish completion script
```

Source the output in your shell profile (e.g. `source <(boid completion bash)`).

## Output formats

Almost every command supports `-o json` for downstream tooling such as `jq`.

```bash
boid task list -o json | jq '.[] | select(.status=="executing")'
boid task show <id> -o yaml
```

## Related documents

- [Getting started](../getting-started/) — guided tutorials.
- [Concepts](../guide/concepts.md) — meanings of task / job / hook / kit / payload / trait.
- [State machine](../guide/state-machine.md) — manual and automatic transition rules.
- [`project.yaml` reference](project-yaml.md) — project-definition fields.
- [Hook script protocol](hook-contract.md) — hook I/O contract.
