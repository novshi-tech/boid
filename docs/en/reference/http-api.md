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
| GET | `/api/projects` | List registered projects. |
| POST | `/api/projects` | Register a project (`{"work_dir": "<path>"}`). |
| POST | `/api/projects/reload` | Re-read every project's `project.yaml`. |
| GET | `/api/projects/{id}/commands` | List the project's `commands`. |
| GET | `/api/projects/{id}/commands/{name}` | Show one command. |
| PUT | `/api/projects/{id}/workspace` | Update workspace assignment. |

For the schema, see [`project.yaml` reference](project-yaml.md). For the CLI, see [CLI / Project](cli.md#project).

### Workspace

| Method | Path | Role |
|---|---|---|
| GET | `/api/workspaces` | List workspaces. |
| GET | `/api/workspaces/{id}` | Workspace detail. |

### Task

| Method | Path | Role |
|---|---|---|
| GET | `/api/tasks` | List tasks (query params: `status`, `behavior`, `workspace_id`). |
| POST | `/api/tasks` | Create a task (body is the JSON form of `taskCreateSpec`). |
| POST | `/api/tasks/import` | Bulk import (JSONL). |
| GET | `/api/tasks/{id}` | Task detail. |
| GET | `/api/tasks/{id}/detail` | Task detail plus actions / jobs (used by the Web UI detail view). |
| PATCH | `/api/tasks/{id}` | Update a task (`UpdateTaskRequest`: `payload` / `instructions` / other fields). |
| DELETE | `/api/tasks/{id}` | Delete a task. |
| POST | `/api/tasks/{id}/duplicate` | Duplicate a task. |
| POST | `/api/tasks/{id}/rerun` | Reset a `done` / `aborted` task to `pending` and re-run. |
| GET | `/api/tasks/{id}/hooks` | List hooks that fire at the task's current status. |
| POST | `/api/tasks/{id}/hooks/{hook_id}/replay` | Replay one hook. |
| GET | `/api/tasks/{id}/events` | **SSE** stream of task events. |
| POST | `/api/tasks/{id}/notify` | Send an agent notification. When `ask` is present, transitions the task to `awaiting`. |
| POST | `/api/tasks/{id}/answer` | Submit a user reply to an `awaiting` task and resume it. |

`POST /api/tasks` request body:

```json
{
  "project_id": "<id>",
  "title": "...",
  "behavior": "<name>",
  "auto_start": true,
  "payload": { ... },
  "instructions": { ... }
}
```

Pass `behavior_spec` instead of `behavior` to specify the behavior inline (see [`project.yaml` / BehaviorSpec](project-yaml.md)).

### Notify and answer

Two endpoints that control the Q&A flow — where an agent pauses to ask the user a question and resumes once answered.

#### `POST /api/tasks/{id}/notify`

Send a notification from the agent to the user. When the `ask` field is present the endpoint enters Q&A mode and transitions the task `executing → awaiting`.

Request body:

```json
{
  "message": "Should I merge PR #42?",
  "ask": "Proceed with the merge?",
  "question_id": "q-550e8400"
}
```

| Field | Required | Description |
|---|---|---|
| `message` | ◎ | Notification text. Passed to the notify script as `BOID_MESSAGE`. |
| `ask` | | Question text. When present, transitions the task to `awaiting`. |
| `question_id` | | UUID for this Q&A turn. Auto-generated when omitted. |

Response: `204 No Content`

Error codes:

| Code | Meaning |
|---|---|
| 400 | `message` is empty |
| 404 | Task not found |
| 501 | `notify.command` is not configured |
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

Append `?follow=true` to wait until the state machine's auto-transitions settle before returning.

### Job

| Method | Path | Role |
|---|---|---|
| GET | `/api/jobs` | List jobs (filter with `task_id`, etc.). |
| GET | `/api/jobs/{id}` | Job detail (status / exit_code / output). |
| POST | `/api/jobs/{id}/done` | (Internal) Notify the daemon that a hook has finished. Accepts `--exit-code` and a payload-patch file. |
| GET | `/api/jobs/{id}/log` | **SSE** stream of live job log. |
| GET | `/api/jobs/{id}/attach/ws` | **WebSocket** to attach to a running runtime (interactive jobs). |

### Secret

| Method | Path | Role |
|---|---|---|
| GET | `/api/secrets` | List keys (values are not returned). |
| POST | `/api/secrets` | Set a secret (key / value in body, namespace via query parameter). |
| DELETE | `/api/secrets/{key}` | Delete a secret. |
| GET | `/api/secrets/{key}/value` | Fetch the value (intended for sandbox / agent callers). |

The namespace is set with `?namespace=...`; defaults to `default` when omitted.

### GC

| Method | Path | Role |
|---|---|---|
| POST | `/api/gc` | Run GC immediately. The body can carry `older_than` and similar parameters. |

The daemon's automatic GC loop runs in the background, so you rarely call this manually. Useful for debugging.

### Web UI management

Pairing and device management for the [Web UI](../guide/web-ui.md). These endpoints sit behind the auth middleware.

| Method | Path | Role |
|---|---|---|
| POST | `/api/web/pair` | Issue a pairing code. |
| GET | `/api/web/devices` | List paired devices. |
| DELETE | `/api/web/devices/{id}` | Revoke one device. |
| DELETE | `/api/web/devices` | Revoke every device. |
| POST | `/api/web/url` | Save the public URL to `config.yaml`. |

The pairing screen at `/login` (HTML) and the cookie-issuing `/auth/redeem` (POST) live alongside these. See [Web UI internals](../architecture/web-internals.md) for the full picture.

### Broker (internal)

Endpoints used by the `boid` shim inside a sandbox to reach back to the host. Not for end users.

| Method | Path | Role |
|---|---|---|
| POST | `/api/broker/register` | Issue a shim token when a hook starts. |

## Server-Sent Events (SSE)

The two SSE endpoints are `/api/tasks/{id}/events` and `/api/jobs/{id}/log`.

### Common

- `Content-Type: text/event-stream`.
- A `:ping\n\n` keepalive every 20 seconds to keep idle proxies from disconnecting.
- Client disconnects (`r.Context().Done()`) cause the request handler to clean up.

### `/api/tasks/{id}/events`

Pushes status changes and payload updates. Each event:

```
event: <kind>
data: <json>

```

For details, see [Web UI internals / SSE](../architecture/web-internals.md#server-sent-events-sse).

### `/api/jobs/{id}/log`

Sends a snapshot (everything captured so far) once, then forwards live runtime stdout/stderr deltas. Ends when the job finishes.

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
