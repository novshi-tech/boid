#!/usr/bin/env bash
set -euo pipefail

e2e_require_cmd curl

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
PROJECT_ID="e2e-web-exec"

# Register project with the initial server (started by run.sh via UNIX socket)
e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

# Restart with a dynamic HTTP address so curl can reach the web UI
e2e_log "stopping initial server"
"$E2E_BIN_DIR/boid" stop
sleep 1

e2e_log "starting boid with dynamic web address"
e2e_run "$E2E_BIN_DIR/boid" start \
  --db-path "$XDG_DATA_HOME/boid/boid.db" \
  --socket-path "$BOID_SOCKET" \
  --kits-dir "$XDG_DATA_HOME/boid/kits" \
  --key-file-path "$XDG_DATA_HOME/boid/boid-secret.key"

"$E2E_BIN_DIR/boid-e2e" wait-health --timeout 15s --interval 100ms "$BOID_SOCKET"

# Ensure this daemon is always stopped on exit (failure or success).
trap '"$E2E_BIN_DIR/boid" stop >/dev/null 2>&1 || true' EXIT

# Get the actual HTTP address from the health endpoint via UNIX socket.
health_json=$(curl -s --unix-socket "$BOID_SOCKET" http://localhost/api/health)
WEB_ADDR=$(printf '%s' "$health_json" | sed -E 's/.*"http_addr":"([^"]+)".*/\1/')
[[ -n "$WEB_ADDR" && "$WEB_ADDR" != "$health_json" ]] || e2e_fail "failed to get web address from health endpoint: $health_json"
e2e_log "web address: $WEB_ADDR"

# Issue pairing code and authenticate
e2e_log "issuing pairing code"
pair_out="$("$E2E_BIN_DIR/boid" web pair)"
printf '%s\n' "$pair_out"
code="$(printf '%s\n' "$pair_out" | grep '^Pairing code:' | sed 's/^Pairing code: //')"
[[ -n "$code" ]] || e2e_fail "failed to parse pairing code from: $pair_out"
e2e_log "pairing code: $code"

e2e_log "redeeming pairing code via /auth?token=<code>"
auth_headers="$(curl -sS -D - -o /dev/null "http://${WEB_ADDR}/auth?token=${code}")"
printf '%s\n' "$auth_headers"
e2e_assert_contains "$auth_headers" "boid_session"
cookie_val="$(printf '%s\n' "$auth_headers" | grep -i 'set-cookie:' | grep 'boid_session' | head -1 | sed -E 's/.*boid_session=([^;]+).*/\1/')"
[[ -n "$cookie_val" ]] || e2e_fail "failed to extract boid_session cookie"
e2e_log "session cookie obtained (length: ${#cookie_val})"

# --- Test 1: GET /api/projects/{id}/commands lists the echo command ---
e2e_log "checking GET /api/projects/${PROJECT_ID}/commands contains echo"
commands_resp="$(curl -sS \
  -H "Cookie: boid_session=${cookie_val}" \
  "http://${WEB_ADDR}/api/projects/${PROJECT_ID}/commands")"
printf '%s\n' "$commands_resp"
e2e_assert_contains "$commands_resp" '"echo"'
e2e_log "OK: echo command listed"

# --- Obtain CSRF token (GET on a protected page sets csrf_token cookie) ---
e2e_log "obtaining CSRF token via GET /projects/${PROJECT_ID}/commands"
csrf_headers="$(curl -sS -D - -o /dev/null \
  -H "Cookie: boid_session=${cookie_val}" \
  "http://${WEB_ADDR}/projects/${PROJECT_ID}/commands")"
printf '%s\n' "$csrf_headers"
csrf_token="$(printf '%s\n' "$csrf_headers" | grep -i 'set-cookie:' | grep 'csrf_token' | head -1 | sed -E 's/.*csrf_token=([^;]+).*/\1/')"
[[ -n "$csrf_token" ]] || e2e_fail "failed to extract csrf_token from response headers"
e2e_log "CSRF token obtained (length: ${#csrf_token})"

# --- Test 2: POST execute → 303 redirect with job_id in Location ---
e2e_log "executing echo command via POST /projects/${PROJECT_ID}/commands/echo/execute"
exec_headers="$(curl -sS -D - -o /dev/null \
  -X POST \
  -H "Cookie: boid_session=${cookie_val}; csrf_token=${csrf_token}" \
  -H "X-CSRF-Token: ${csrf_token}" \
  "http://${WEB_ADDR}/projects/${PROJECT_ID}/commands/echo/execute")"
printf '%s\n' "$exec_headers"
location="$(printf '%s\n' "$exec_headers" | grep -i '^location:' | head -1 | sed -E 's/[Ll]ocation:[[:space:]]*//' | tr -d '\r')"
job_id="$(printf '%s\n' "$location" | sed -E 's|.*/jobs/([^/]+)/.*|\1|')"
[[ -n "$job_id" ]] || e2e_fail "failed to extract job_id from Location header: '$location'"
e2e_log "job_id: $job_id"

# --- Test 3: WebSocket output contains hello-from-web ---
e2e_log "checking WebSocket output contains 'hello-from-web'"
"$E2E_BIN_DIR/boid-e2e" ws-job-output \
  --addr "http://${WEB_ADDR}" \
  --job "$job_id" \
  --cookie "boid_session=${cookie_val}" \
  --timeout 10s \
  --contains "hello-from-web"
e2e_log "OK: 'hello-from-web' received via WebSocket"

# --- Test 4: job reaches completed status ---
e2e_log "waiting for job ${job_id} to reach completed status"
deadline=$((SECONDS + 10))
while true; do
  job_resp="$(curl -sS "http://${WEB_ADDR}/api/jobs/${job_id}" 2>/dev/null || true)"
  if printf '%s\n' "$job_resp" | grep -q '"completed"'; then
    break
  fi
  if (( SECONDS >= deadline )); then
    e2e_fail "job ${job_id} did not reach completed status within 10s (last resp: $job_resp)"
  fi
  sleep 0.2
done
e2e_log "OK: job ${job_id} is completed"
