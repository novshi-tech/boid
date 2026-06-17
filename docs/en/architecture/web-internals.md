# Web UI internals

What happens between an HTTP request landing on `boid` and the response going back, at the granularity of authentication, sessions, and SSE. The detailed zoom of the Web UI section in the [Architecture overview](overview.md).

The intended readers are contributors who touch `internal/api/auth/` or `internal/api/events_handler.go` and friends. For the user-facing introduction, see [Web UI](../guide/web-ui.md).

## Components

```
+----------------------+        +-----------------------+
| Browser (or phone)   |        | boid daemon (HTTP)    |
|                      |        |                       |
|  GET /tasks          | -----> |  chi router           |
|  POST /tasks/<id>... |  HTTPS |    └─ middleware:     |
|  EventSource /events |        |       ├─ WebAuth      |
|                      |        |       ├─ CSRF         |
|                      | <----- |       └─ Templ + chi  |
|  cookies:            |        |                       |
|    boid_session      |        |  TaskEventHub (SSE)   |
|    csrf_token        |        |  RuntimeSubscriber    |
+----------------------+        +-----------------------+
                                          |
                                          v
                                +-----------------------+
                                | SQLite                |
                                |   web_devices         |
                                |   web_pairing_codes   |
                                +-----------------------+
```

Where the code lives:

- Router assembly: [`internal/server/server.go`](https://github.com/novshi-tech/boid/blob/main/internal/server/server.go) and `internal/server/wire.go`.
- Auth middleware and pairing: [`internal/api/auth/`](https://github.com/novshi-tech/boid/tree/main/internal/api/auth).
- HTTP handlers: `internal/api/web.go`, `internal/api/web_deps.go`.
- SSE: `internal/api/events_handler.go` (task events), `internal/api/job_log_sse.go` (job logs).
- Templates: `web/templates/` (Templ); static assets: `web/static/`.

## Auth middleware

[`internal/api/auth/middleware.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/auth/middleware.go) `NewWebAuthMiddleware` checks the session cookie:

| Cookie | Origin | Outcome |
|---|---|---|
| absent | loopback AND no devices in DB | log a warning and pass (**bootstrap mode**) |
| absent | anything else | `302 /login` |
| invalid | (any) | clear the cookie and `302 /login` |
| valid | (any) | pass; update the device's `last_seen_at` |

Exempt paths (skip the check entirely): `/login`, `/auth*`, `/static/*`.

### Why bootstrap mode is safe

The only path that lets a request through without a cookie is "loopback request, no devices yet registered". This is what lets you open `http://localhost:8080` immediately after `boid start` and reach the pairing screen.

To prevent fake-loopback through reverse proxies, `IsLoopback` in [`internal/api/auth/loopback.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/auth/loopback.go) treats the request as **not** loopback if any of `X-Forwarded-For`, `CF-Connecting-IP`, or `Forwarded` is present. A Cloudflare Tunnel terminating on localhost still adds those headers, so the bootstrap exemption never fires through a tunnel.

## Session cookie

[`internal/api/auth/session_store.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/auth/session_store.go) `SessionSigner` issues and verifies cookies.

### Cookie format

```
boid_session = <deviceID> "." <hex(HMAC-SHA256(secret, deviceID))>
```

- `<deviceID>` is `web_devices.id` (a UUID).
- `<sig>` is HMAC-SHA256, hex-encoded.
- The `secret` is loaded at daemon startup from `~/.local/share/boid/web_secret` (mode 0600).

Cookie attributes: `Path=/; HttpOnly; Secure; SameSite=Lax; Max-Age=7776000` (90 days). The 30-day idle cap is enforced by checking `last_seen_at` in the database (currently updated on every request).

### Verify

1. Read the `boid_session` cookie.
2. Split at the last `.` into `deviceID` and `sig`.
3. Recompute `HMAC-SHA256(secret, deviceID)` and compare with `sig` using `hmac.Equal` (timing-safe).
4. Look up `web_devices` for an active row with that ID (`revoked_at IS NULL`).
5. Update `last_seen_at` to `now()`.

A bad cookie or a cookie for a revoked device gets cleared, and the request is redirected to `/login`.

## Pairing flow

[`internal/api/auth/pairing.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/auth/pairing.go) `PairingManager` handles code issue and redeem.

```
host (CLI)               daemon (HTTP)              browser
========================================================
boid web pair      → POST /api/web/pair
                  ←  code: WX7K-4QJP
displays
"WX7K-4QJP"
                                                    GET /login
                                                    enter code
                                                    POST /login  (form submit)
                  ← session cookie + 302 /

                         -- or via magic link --

boid web pair      → POST /api/web/pair
                  ←  magic link URL
                                                    GET /auth?token=<token>
                  ← session cookie + 302 /
```

### Issue

1. Generate 8 random alphanumeric characters with `crypto/rand`, with a hyphen in the middle: `WX7K-4QJP`.
2. SHA-256 hash, insert into `web_pairing_codes(code_hash, label, created_at, expires_at)`.
3. `expires_at = now() + 5 minutes`. Single use — once `consumed_at` becomes non-NULL the code can no longer be redeemed.

### Redeem

1. SHA-256 hash the submitted code.
2. Look up `web_pairing_codes`. Reject if `expires_at < now()` or `consumed_at != NULL`.
3. Atomically update `consumed_at = now()`.
4. Insert a new row into `web_devices`.
5. Issue a session cookie via `SessionSigner.Issue` for that device ID.

### Rate limiting

[`internal/api/auth/ratelimit.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/auth/ratelimit.go):

- 5 attempts per 5-minute window per IP.
- 15-minute lockout when exceeded.
- Locked IPs get `429` immediately.
- In-memory only — resets when the daemon restarts.

Applies to the pairing screen and the redeem endpoint.

## CSRF

[`internal/api/auth/csrf.go`](https://github.com/novshi-tech/boid/blob/main/internal/api/auth/csrf.go) implements **double-submit cookie**.

### GET / HEAD / OPTIONS / TRACE

- If the `csrf_token` cookie is absent, generate a random value and set it.
- Pass through.

### POST / PUT / PATCH / DELETE

1. Require the `csrf_token` cookie (403 if missing).
2. Require the `X-CSRF-Token` header to match the cookie value (403 if missing or mismatched).

The JS layer (HTMX) is configured via `hx-headers` to send `X-CSRF-Token`. The check looks only at the cookie / header agreement — it does not inspect `Origin` or `Referer` — so it works behind a reverse proxy.

## Server-Sent Events (SSE)

`boid` exposes two SSE endpoints.

### Task events (`GET /api/tasks/{id}/events`)

Handler: `WebHandler.TaskEvents` in `internal/api/events_handler.go`. Pushes task status changes and payload updates to the browser.

Implementation notes:

- Response headers: `Content-Type: text/event-stream`, `Cache-Control: no-cache`, `Connection: keep-alive`.
- `h.Hub.Subscribe(ctx, taskID)` subscribes to the `TaskEventHub` (an in-memory pub/sub).
- The loop:
  - On a received event: write `event: <kind>\ndata: <json>\n\n` and flush.
  - Every 20 seconds: write `:ping\n\n` to keep idle proxies from disconnecting.
  - `r.Context().Done()` detects client disconnect, ending the handler.

`TaskEventHub` is implemented separately (e.g. `internal/api/task_event_hub.go`); the dispatch loop publishes events whenever it advances a task.

### Job log (`GET /api/jobs/{id}/log`)

Handler: `JobLogSSEHandler` in `internal/api/job_log_sse.go`. Streams the live stdout/stderr of a running hook.

- Without `?follow=true`: returns a plain-text snapshot of the log captured so far (`text/plain`).
- With `?follow=true`: upgrades to SSE (`text/event-stream`). A snapshot is sent first, then subsequent stdout/stderr deltas come through `RuntimeSubscriber.Subscribe`. SSE format is `data: <line>` only (no `event:` field). `:ping` keepalive is sent every 20 seconds.

When the job ends, the subscriber channel is closed and the SSE connection terminates.

## Route mounting

[`internal/server/wire.go`](https://github.com/novshi-tech/boid/blob/main/internal/server/wire.go) wires the routes. A representative shape:

```
chi.Router
├── /static/*                             (static assets)
├── /login                                (GET: pairing screen; POST: redeem code via form)
├── /auth                                 (GET: redeem magic link token, ?token=<token>)
├── /api/web/pair                         (POST: issue a pairing code)
├── /api/web/devices                      (GET: list devices)
├── /api/web/devices/{id}                 (DELETE: revoke a device)
├── (everything below is behind WebAuthMiddleware + CSRFMiddleware)
├── /                                     (task list)
├── /tasks/new                            (new task form)
├── /tasks/{id}                           (task detail)
├── /tasks/{id}/edit                      (edit task)
├── /tasks/{id}/questions/{question_id}   (Q&A page)
├── /tasks/{id}/hooks                     (hook replay list)
├── /sessions                             (session list)
├── /sessions/new                         (new session form)
├── /jobs/{id}                            (job detail)
├── /api/tasks/{id}/events                (SSE: task events)
└── /api/jobs/{id}/log                    (SSE or snapshot: job log)
```

> **Note:** There is no `/auth/redeem`, `/auth/logout`, or `/projects/<id>` page route. Project detail is an API endpoint (`/api/projects/{id}`), not a web page.

## Related documents

- [Web UI](../guide/web-ui.md) — user-facing pairing and Cloudflare Tunnel.
- [Architecture overview](overview.md) — where the Web UI layer sits.
- [Persistence layer](persistence.md) — definitions of `web_devices` and `web_pairing_codes`.
