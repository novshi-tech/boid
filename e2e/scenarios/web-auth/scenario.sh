#!/usr/bin/env bash
set -euo pipefail

e2e_require_cmd curl

# Web UI is always enabled. Restart with a dynamic addr so curl can reach it.
e2e_log "stopping default server"
"$E2E_BIN_DIR/boid" stop

# Give the daemon 50 ms grace + margin to fully shut down before restarting.
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

# --- Test 1: loopback access without cookie (no devices registered) → 200 ---
e2e_log "loopback access without cookie, no devices → expect 200 (bootstrap exemption)"
status_no_device="$(curl -sS -o /dev/null -w '%{http_code}' "http://${WEB_ADDR}/")"
printf 'status: %s\n' "$status_no_device"
[[ "$status_no_device" == "200" ]] || e2e_fail "expected 200 for loopback no-cookie no-devices, got $status_no_device"

# --- Test 2: issue pairing code ---
e2e_log "issuing pairing code via boid web pair"
pair_out="$("$E2E_BIN_DIR/boid" web pair)"
printf '%s\n' "$pair_out"
code="$(printf '%s\n' "$pair_out" | grep '^Pairing code:' | sed 's/^Pairing code: //')"
[[ -n "$code" ]] || e2e_fail "failed to parse pairing code from boid web pair output"
e2e_log "pairing code: $code"

# --- Test 3: redeem code via GET /auth?token=<code> → Set-Cookie ---
e2e_log "redeeming pairing code via /auth?token=<code>"
auth_headers="$(curl -sS -D - -o /dev/null "http://${WEB_ADDR}/auth?token=${code}")"
printf '%s\n' "$auth_headers"
e2e_assert_contains "$auth_headers" "Set-Cookie"
e2e_assert_contains "$auth_headers" "boid_session"

# Extract the raw cookie value (device_id.sig) for subsequent requests.
# The Set-Cookie header is: boid_session=<value>; Path=/; ...
cookie_val="$(printf '%s\n' "$auth_headers" | grep -i 'set-cookie:' | head -1 | sed -E 's/.*boid_session=([^;]+).*/\1/')"
[[ -n "$cookie_val" ]] || e2e_fail "failed to extract boid_session cookie value from Set-Cookie header"
e2e_log "session cookie extracted (length: ${#cookie_val})"

# --- Test 4: access / with valid session cookie → 200 ---
# Pass cookie via header to bypass curl's Secure-flag restriction on HTTP.
e2e_log "accessing / with session cookie → expect 200"
status_with_cookie="$(curl -sS -o /dev/null -w '%{http_code}' \
  -H "Cookie: boid_session=${cookie_val}" \
  "http://${WEB_ADDR}/")"
printf 'status: %s\n' "$status_with_cookie"
[[ "$status_with_cookie" == "200" ]] || e2e_fail "expected 200 with valid cookie, got $status_with_cookie"
