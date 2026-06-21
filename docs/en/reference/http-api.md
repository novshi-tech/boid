# HTTP API reference

The `/api/*` endpoints exposed by the `boid` daemon. The main consumers are the CLI (`boid` commands) and the Web UI; scripts that talk to the daemon directly use the same surface.

This page is the index. It does not document every JSON field — for that, read the types in [`internal/orchestrator/model.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/model.go) and [`internal/orchestrator/spec_types.go`](https://github.com/novshi-tech/boid/blob/main/internal/orchestrator/spec_types.go).

## Access paths

The `boid` daemon listens on two transports:

| Transport | Address | Authentication |
|---|---|---|
| UNIX socket (CLI) | `$XDG_RUNTIME_DIR/boid.sock` (fallback `/tmp/boid-<uid>.sock`) | OS file permissions — trusted (CLI and in-sandbox agents) |
| HTTP listener (Web UI) | `127.0.0.1:8080` (default; change with `boid web set-addr`) | Device session; data/control `/api/*` require auth over TCP |

The CLI and in-sandbox agents send `/api/*` requests through the UNIX socket, which is trusted (filesystem permissions) and never gated.

Over the HTTP/TCP listener the data and control `/api/*` endpoints require a valid `boid_session` cookie. Two exceptions: `/api/health` is public, and a loopback bootstrap exemption lets a genuine local browser through until the first device is paired (`boid web pair`). Requests forwarded by a reverse proxy / tunnel get no bootstrap. Unauthenticated calls receive `401`. `/api/*` is CSRF-exempt; the `boid_session` cookie is `SameSite=Lax`, which blocks cross-site requests.

The listener binds to **loopback only** by default. Expose it deliberately (`boid web set-addr <addr>`, then a tunnel or reverse proxy) rather than binding all interfaces.

A curl example over the UNIX socket:

```bash
curl --unix-socket "$XDG_RUNTIME_DIR/boid.sock" http://localhost/api/health
```

## Conventions

### Requests / responses

- Request bodies and successful responses are JSON.
- POST / PUT / PATCH expect `Content-Type: application/json`.
- Error responses are `{"error": "<message>"}`.

### IDs

- `<id>` (task IDs and the like) is a UUID string.
- `<project-ref>` matches the project's `id` exactly or the `name` partially.

## Endpoints

### Server management

| Method | Path | Role |
|---|---|---|
| GET | `/api/health` | Health check (200 = alive). |
| POST | `/api/shutdown` | Stop the daemon (used by `boid stop`). |
| GET | `/api/proxy` | Metadata for the sandbox-facing HTTP proxy. |

### Project

| Method | Path | Role |
|---|---|---|
| GET | `/api/projects` | List registered projects (query: `workspace_id`). |
| POST | `/api/projects` | Register a project (`{"work_dir": "<path>"}`). |
| GET | `/api/projects/{id}` | Project detail. |
| DELETE | `/api/projects/{id}` | Unregister a project. |
| POST | `/api/projects/reload` | Re-read every project's `project.yaml`. |
| GET | `/api/projects/{id}/commands` | List the project's `commands`. |
| GET | `/api/projects/{id}/commands/{name}` | Show one command. |
| POST | `/api/projects/{id}/commands/{name}/execute` | Execute a named command. |
| PUT | `/api/projects/{id}/workspace` | Update workspace assignment. |

For the schema, see [`project.yaml` reference](project-yaml.md). For the CLI, see [CLI / Project](cli.md#project).

### Workspace

| Method | Path | Role |
|---|---|---|
| GET | `/api/workspaces` | List workspaces. |

### Task

| Method | Path | Role |
|---|---|---|
| GET | `/api/tasks` | List tasks (query params: `status`, `behavior`, `workspace_id`, `project_id`). |
| POST | `/api/tasks` | Create a task (body is the JSON form of `taskCreateSpec`). |
| POST | `/api/tasks/import` | Bulk import (JSONL). |
| GET | `/api/tasks/{id}` | Task detail. |
| GET | `/api/tasks/{id}/detail` | Task detail plus actions / jobs (used by the Web UI detail view). |
| PATCH | `/api/tasks/{id}` | Update a task (`UpdateTaskRequest`: `payload` / `instructions` / other fields). |
| DELETE | `/api/tasks/{id}` | Delete a task (`?force=true` to skip state checks). |
| POST | `/api/tasks/{id}/duplicate` | Duplicate a task. |
| POST | `/api/tasks/{id}/rerun` | Reset a `done` / `aborted` task to `pending` and re-run. |
| GET | `/api/tasks/{id}/hooks` | List hooks that fire at the task's current status (query: `status`). |
| GET | `/api/tasks/{id}/field` | Fetch a single field value from the task. |
| GET | `/api/tasks/{id}/commands` | List the task's project commands. |
| POST | `/api/tasks/{id}/commands/{name}/execute` | Execute a named project command in the task's context. |
| POST | `/api/tasks/{id}/hooks/{hook_id}/replay` | Replay one hook. |
| GET | `/api/tasks/{id}/events` | **SSE** stream of task events. |
| POST | `/api/tasks/{id}/notify` | Send an agent notification. When `ask` is present, transitions the task to `awaiting`. |
| POST | `/api/tasks/{id}/answer` | Submit a user reply to an `awaiting` task and resume it. |

`POST /api/tasks` request body:

```json
{
  "id": "<uuid>",
  "project_id": "<id>",
  "title": "...",
  "description": "...",
  "behavior": "<name>",
  "auto_start": true,
  "payload": { ... },
  "instructions": { ... },
  "remote_id": "...",
  "traits": { ... },
  "ref": "...",
  "parent_id": "<uuid>"
}
```

Pass `behavior_spec` instead of `behavior` to specify the behavior inline (see [`project.yaml` / BehaviorSpec](project-yaml.md)). `id` lets callers supply a deterministic UUID (idempotent create). `parent_id` links the task to a parent task.

### Notify and answer

Two endpoints that control the Q&A flow — where an agent pauses to ask the user a question and resumes once answered.

#### `POST /api/tasks/{id}/notify`

Send a notification from the agent to the user. When the `ask` field is present the endpoint enters Q&A mode and transitions the task `executing → awaiting`.

Request body:

```json
{
  "message": "Should I merge PR #42?",
  "ask": "Proceed with the merge?",
  "question_id": "q-550e8400",
  "progress": true,
  "done": true,
  "fail": true
}
```

| Field | Required | Description |
|---|---|---|
| `message` | ◎ (except when `progress` is set) | Notification text. Passed to the notify script as `BOID_MESSAGE`. |
| `ask` | | Question text. When present, transitions the task to `awaiting`. The daemon **no longer dispatches a resume hook** on answer — use `boid task ask` (blocking RPC) for any real Q&A. Mutually exclusive with `done`/`fail`/`progress`. |
| `question_id` | | UUID for this Q&A turn. Auto-generated when omitted. |
| `progress` | | When `true`, records a progress entry on the timeline only (no state transition). |
| `done` | | When `true`, records a `done_request` on the timeline; the task transitions to `done` after the runtime exits. |
| `fail` | | When `true`, records a `fail_request` on the timeline; the task transitions to `aborted` after the runtime exits. |

`ask`, `done`, `fail`, and `progress` are mutually exclusive. FYI notifications (none of the above) from child tasks are silently dropped — only root-task FYI fires the notify hook.

Response: `204 No Content`

Error codes:

| Code | Meaning |
|---|---|
| 400 | `message` is empty (when required) |
| 404 | Task not found |
| 501 | `notify.command` is not configured (rare; service is always wired) |
| 409 | `ask` was given but the task is not in `executing` state |

#### `POST /api/tasks/{id}/answer`

Submit the user's reply to an `awaiting` task. Stores the answer in `payload.awaiting.pending_answer` and transitions the task `awaiting → executing`, which restarts the hook.

Request body:

```json
{
  "question_id": "q-550e8400",
  "answer": "yes"
}
```

| Field | Required | Description |
|---|---|---|
| `question_id` | ◎ | UUID of the Q&A turn being answered |
| `answer` | ◎ | Answer text |

Response: `204 No Content`

Error codes:

| Code | Meaning |
|---|---|
| 400 | `question_id` or `answer` is empty |
| 404 | Task not found |
| 409 | Task is not in `awaiting` state |

### Action

Issue a state transition for a task.

| Method | Path | Role |
|---|---|---|
| POST | `/api/tasks/{taskID}/actions` | Send an action. |

Body:

```json
{
  "type": "start",
  "payload": { ... }
}
```

`type` is one of `start`, `done`, `reopen`, `ask`, `answer`, `abort`. `payload` is optional metadata. For the `ask` / `answer` operations the dedicated `/notify` / `/answer` endpoints above are simpler to use.

### Job

| Method | Path | Role |
|---|---|---|
| GET | `/api/jobs` | List jobs (query: `task_id`, `status`, `interactive`, `taskless`). |
| GET | `/api/jobs/{id}` | Job detail (status / exit_code / output). |
| PATCH | `/api/jobs/{id}` | Update job metadata (e.g. `display_name`). |
| POST | `/api/jobs/{id}/done` | (Internal) Notify the daemon that a hook has finished. Accepts `exit_code` and `output` in body; a payload-patch file may be included. |
| GET | `/api/jobs/{id}/log` | Job log. Without `?follow=true` returns a `text/plain` snapshot. With `?follow=true` streams as **SSE** (`data: <line>` events only; no `event:` field). |
| GET | `/api/jobs/{id}/attach/ws` | **WebSocket** to attach to a running runtime (interactive jobs). |
| POST | `/api/jobs/{id}/agent-stop` | Send a stop signal to the agent running this job. |
| POST | `/api/jobs/{id}/attach` | (Internal) Attach a PTY/pipe to a running job. |
| POST | `/api/jobs/{id}/resize` | Resize the PTY for a running interactive job. |

### Secret

| Method | Path | Role |
|---|---|---|
| GET | `/api/secrets` | List keys (values are not returned). Namespace via query `?namespace=`. |
| POST | `/api/secrets` | Set a secret. Body: `{"key": "...", "value": "...", "namespace": "..."}`. |
| DELETE | `/api/secrets/{key}` | Delete a secret. Namespace via query `?namespace=`. |
| GET | `/api/secrets/{key}/value` | Fetch the value (intended for sandbox / agent callers). |

For `POST`, namespace is a **body field** (not a query parameter). For `GET` and `DELETE`, namespace is a **query parameter** (`?namespace=`). Both default to `default` when omitted.

### GC

| Method | Path | Role |
|---|---|---|
| POST | `/api/gc` | Run GC immediately. The body can carry `older_than`, `dry_run`, and similar parameters. |

The daemon's automatic GC loop runs in the background, so you rarely call this manually. Useful for debugging. When `dry_run` is `true`, reports what would be deleted without removing anything.

### Web UI management

Pairing and device management for the [Web UI](../guide/web-ui.md). These endpoints sit behind the auth middleware.

| Method | Path | Role |
|---|---|---|
| POST | `/api/web/pair` | Issue a pairing code. Optional body: `{"label": "...", "expires_in": "<duration>"}`. |
| GET | `/api/web/devices` | List paired devices. |
| DELETE | `/api/web/devices/{id}` | Revoke one device. |
| DELETE | `/api/web/devices` | Revoke every device. |

The pairing screen at `/login` (HTML) and the cookie-issuing `/auth/redeem` (POST) live alongside these. See [Web UI internals](../architecture/web-internals.md) for the full picture.

### Broker (internal)

Endpoints used by the `boid` shim inside a sandbox to reach back to the host. Not for end users.

| Method | Path | Role |
|---|---|---|
| GET | `/api/broker` | List active broker tokens / shim connections. |
| POST | `/api/broker/register` | Issue a shim token when a hook starts. |

## Server-Sent Events (SSE)

The primary SSE endpoint is `/api/tasks/{id}/events`. `/api/jobs/{id}/log` supports an optional SSE mode.

### Common (SSE endpoints)

- `Content-Type: text/event-stream`.
- A `:ping\n\n` keepalive every 20 seconds to keep idle proxies from disconnecting (events endpoints only).
- Client disconnects (`r.Context().Done()`) cause the request handler to clean up.

### `/api/tasks/{id}/events`

Pushes status changes and payload updates. Each event:

```
event: <kind>
data: <json>

```

For details, see [Web UI internals / SSE](../architecture/web-internals.md#server-sent-events-sse).

### `/api/jobs/{id}/log`

- **Without `?follow=true`**: returns a `text/plain` snapshot of the log captured so far and closes.
- **With `?follow=true`**: streams as SSE. Format is `data: <line>` only — no `event:` field. `:ping` keepalive is **not** sent on this endpoint. Ends when the job finishes.

## Error format

Failures return an HTTP status code with a JSON body:

```json
{
  "error": "task not found"
}
```

Common status codes:

| Code | Meaning |
|---|---|
| 400 | Malformed request. |
| 403 | CSRF / web-auth check failed (HTTP listener only). |
| 404 | Resource not found. |
| 409 | State-machine precondition violated (e.g. an action from a terminal status). |
| 500 | Internal error. |

## Related documents

- [CLI reference](cli.md) — the CLI commands that hit each endpoint.
- [`project.yaml` reference](project-yaml.md) — schemas required when creating tasks.
- [Hook script protocol](hook-contract.md) — what the EXIT trap calls when it hits `POST /api/jobs/{id}/done`.
- [Web UI internals](../architecture/web-internals.md) — auth middleware, SSE, and the route mount layout.
