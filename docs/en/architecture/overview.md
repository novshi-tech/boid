# Architecture overview

A guide to how the source is laid out and how the pieces fit together, aimed at contributors who are reading the code for the first time.

This is not an exhaustive `internal/` reference — it is the picture you need in your first hour to navigate the codebase. For the details, read the `*.go` files in each package.

## Process layout

```
┌──────────┐  UNIX socket   ┌────────────────┐
│ boid CLI │ ─────────────► │  boid daemon   │
└──────────┘                │ (long-running  │
                            │  server)       │
                            │ ┌────────────┐ │
                            │ │ broker     │ │
                            │ └────────────┘ │
                            │      ▲         │
                            └──────┼─────────┘
                                   │ stdin/stdout
                                   │
                          ┌────────┴────────┐
                          │ sandboxed hook  │
                          │ / gate script   │
                          └─────────────────┘
```

Three kinds of process are involved:

- **CLI (`boid`)** — the command users invoke. By default it is just a thin client that goes over the UNIX socket to the daemon and prints the response. The actual work happens in the daemon.
- **Daemon** (started by `boid start`) — the long-running server. Owns the SQLite database, accepts CLI / Web UI requests, drives the state machine, and launches hooks and gates. One per host.
- **Broker + sandboxed hook/gate scripts** — the daemon spawns scripts inside a sandbox; the broker forwards requests that need to cross the sandbox boundary (host commands, `boid task update`) back to the host.

The CLI also auto-starts the daemon: any subcommand that needs the socket starts `boid` in the background if it is not already running ([`internal/client/autostart.go`](https://github.com/novshi-tech/boid/blob/main/internal/client/autostart.go)).

## Source tree

```
cmd/                  - cobra-based CLI commands (boid task, boid project, ...)
main.go               - entry point; calls cmd.Execute()
internal/
  client/             - UNIX-socket HTTP client for the daemon, plus autostart
  daemon/             - daemonization (Spawn / WaitForSocket / RedirectToLog)
  config/             - reads ~/.config/boid/config.yaml
  db/                 - SQLite handle + migrations
  server/             - HTTP / UNIX listeners and chi router wiring
  api/                - HTTP handlers and TaskWorkflowService
  orchestrator/       - state machine, ProjectStore, persistence and evaluation
  dispatcher/         - hook/gate job launching, sandbox plan building, worktree management
  sandbox/            - mount namespace + chroot, host-command broker, HTTP proxy
  kit/                - cloning, loading, and detection for kit repositories
  tui/                - bubbletea-based TUI
  initwizard/         - interactive setup for `boid init`
  logrotate/          - size-based rotation for the daemon log
  qrterm/             - terminal-side QR rendering
  skills/             - bundled Claude Code skills
  timeline/           - task timeline view
web/                  - Templ templates + static assets
testutil/             - test helpers
e2e/                  - black-box E2E (scenarios + fixture kit + fake host commands)
```

## Layering

The four core layers in `internal/` import each other in a strict direction:

```
        ┌─────────┐
        │ sandbox │ ← leaf (no internal deps)
        └─────────┘
             ▲
             │
       ┌─────────────┐
       │ dispatcher  │ → also depends on orchestrator
       └─────────────┘
             ▲
             │
       ┌─────────────┐
       │ orchestrator│ → only depends on db (no dispatcher / sandbox)
       └─────────────┘
             ▲
             │
        ┌────────┐
        │ api /  │ → wires everything together
        │ server │
        └────────┘
```

Two design constraints worth knowing about:

- **`orchestrator` must not depend on `dispatcher` or `sandbox`.** Orchestrator owns the domain logic (state machine, task / job / project evaluation) and stays unaware of execution details.
- **`dispatcher` is the bridge.** It translates between what orchestrator decides ("run this hook next") and what sandbox needs (a primitive plan: mount this, allow that command, run this argv).
- **`sandbox` does not look at orchestrator types.** Everything sandbox needs to act on is passed in as primitives (BindMount lists, CommandDef lists, ...) by dispatcher.

This boundary has been broken once during a large refactor; we now check it mechanically during reviews. See [Contributing](../contributing/README.md) for how.

## The major components

### internal/server

A thin assembly layer for the daemon. `New()`:

1. Opens SQLite and runs migrations.
2. Initialises `orchestrator.ProjectStore`.
3. Starts the `sandbox.Broker`.
4. Builds the dispatcher runtime.
5. Mounts API handlers on the chi router.

`Start()` then brings up the UNIX socket, the TCP listener, the HTTP server, and the GC loop in order.

Entry: [`internal/server/server.go`](https://github.com/novshi-tech/boid/blob/main/internal/server/server.go).

### internal/api

The HTTP handlers and the `TaskWorkflowService` that backs them. When the daemon receives a task action, task creation, or task update request, the service runs `runDispatchLoop` to drive the state machine forward.

`runDispatchLoop` does:

1. Call `orchestrator.Coordinator.DispatchAndAdvance`.
2. The coordinator picks the hooks/gates that match the current status, runs them through the dispatcher, applies the returned payload patches, and re-evaluates the state machine's auto-transition rules.
3. If the status changed, loop. If not, return.

Entry: [`internal/api/service.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/service.go).

### internal/orchestrator

The domain layer.

- **State machine** (`machine.go`) — rules for `pending → executing → done`, auto-transitions, abort conditions.
- **Coordinator** (`coordinator.go`) — runs one dispatch + advance step.
- **Evaluator** (`evaluator.go`) — picks which hooks/gates fire.
- **ProjectStore** (`project_store.go`) — in-memory cache of projects with kit metadata resolved.
- **lifecycle / payload merge / blocked / readonly** — computed traits and helpers used in transition rules.

Because it does not depend on dispatcher or sandbox, the state machine is fully unit-testable.

### internal/dispatcher

The bridge layer for job execution.

- **broker** — wraps `sandbox.Broker` and exposes `RunJob` for running one hook or gate.
- **sandbox_builder** — turns `orchestrator.ProjectMeta` into a primitive sandbox plan.
- **policy_translate** — maps a kit's `host_commands` declaration to sandbox-side `CommandDef`s.
- **runner / runtime** — runs jobs and collects results.
- **worktree_manager** — creates, recreates, and tears down git worktrees for behaviors with `worktree: true`.
- **secret_store** — encrypts and serves stored secret values.

Entry: [`internal/dispatcher/runner.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/runner.go), [`internal/dispatcher/worktree_manager.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/worktree_manager.go).

### internal/sandbox

The Linux sandbox itself.

- **broker** — listens on a UNIX socket inside the daemon and runs requests from inside the sandbox (host commands, `boid task update`, ...) on the host.
- **proxy** — outbound HTTP proxy that allows only the domains the kit declares.
- **mount namespace + chroot** — uses `unshare(2)` and bind mounts to confine reads and writes.
- **boid shim** — the `boid` binary visible inside the sandbox; effectively a thin client to the broker.

Does not see orchestrator types (the layering rule). Inputs come in as primitives prepared by dispatcher.

### internal/kit

Clones kit repositories, reads `kit.yaml`, and runs `detect.sh`.

Entry: [`internal/kit/registry.go`](https://github.com/novshi-tech/boid/blob/main/internal/kit/registry.go).

### internal/db

SQLite handle and migrations. Uses `modernc.org/sqlite` (a pure-Go SQLite implementation), so no cgo is required.

## A single action, end to end

What happens when a user runs `boid action send --task <id> --type start`:

1. **CLI:** `cmd/action.go` calls `client.NewUnixClient(...)` and POSTs `/api/tasks/<id>/actions`.
2. **Daemon HTTP:** an `internal/api` handler receives the request and calls `TaskWorkflowService.ApplyAction`.
3. **State machine:** `orchestrator.StateMachine.ApplyAction` evaluates the `start` rule and returns `pending → executing`.
4. **Persistence:** the new status and lifecycle are written to SQLite.
5. **Dispatch loop:** `runDispatchLoop` kicks in and calls `Coordinator.DispatchAndAdvance`.
6. **Hook selection:** `Evaluator` picks the hooks bound to this behavior in the kit metadata. Hooks always fire while the task is in `executing`.
7. **Execution:** dispatcher assembles the sandbox plan and calls `sandbox.Broker.RunJob` to launch the hook script.
8. **Payload merge:** the hook's stdout is parsed as JSON and the `payload_patch` is merged into the payload.
9. **Auto-transition:** the state machine re-evaluates; any matching auto-transition fires another round.
10. **Response:** the final task state is returned as JSON; the CLI prints it.

Job logs (stderr) are stored in SQLite and surfaced via `boid job show <job-id>`.

## Where to look

| If you want to ... | Start at ... |
|---|---|
| Change a state-machine rule | [`internal/orchestrator/machine.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/machine.go) |
| Change which hooks/gates fire | [`internal/orchestrator/evaluator.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/evaluator.go) |
| Trace the whole dispatch cycle | `runDispatchLoop` in [`internal/api/service.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/service.go) |
| Trace a single job | [`internal/dispatcher/runner.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/runner.go) |
| Worktree handling | [`internal/dispatcher/worktree_manager.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/worktree_manager.go) |
| Host command policy | [`internal/dispatcher/policy_translate.go`](https://github.com/novshi-tech/boid/blob/main/internal/dispatcher/policy_translate.go) |
| Sandbox boundary | [`internal/sandbox/broker.go`](https://github.com/novshi-tech/boid/blob/main/internal/sandbox/broker.go) |
| Web UI auth | files under [`internal/api/`](https://github.com/novshi-tech/boid/blob/main/internal/api/) prefixed `web_auth_` |
| The whole TUI | [`internal/tui/app.go`](https://github.com/novshi-tech/boid/blob/main/internal/tui/app.go) |

## Related docs

- [Concepts](../guide/concepts.md) — vocabulary.
- [State machine](../guide/state-machine.md) — states and transitions.
- [`project.yaml` reference](../reference/project-yaml.md) — schema for project definitions.
- [Kit authoring overview](../kit-authoring/overview.md) — the kit I/O protocol.
- [Contributing](../contributing/README.md) — development workflow.
