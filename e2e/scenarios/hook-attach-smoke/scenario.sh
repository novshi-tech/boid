#!/usr/bin/env bash
set -euo pipefail

e2e_require_cmd curl

# Scenario: hook job (Foreground=false) で割り当てられた PTY に Web UI 経由で
# WebSocket attach し、 hook の stdout がライブで届くことを確認する。
#
# 退行確認の対象は、 PlanHook で立てた spec.Interactive=true が
# sandbox_builder の TTY → RuntimeStartSpec.Interactive を経由して
# LocalRuntime の PTY 割り当てに正しく流れていること。 web-commands-exec は
# 同じ WS attach 経路を Foreground=true (boid exec) で検証しているため、
# このシナリオは hook 経路 (Foreground=false + EXIT trap) を担当する。

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
PROJECT_ID="hook-attach-smoke"

# Set up workspace for new schema (PR4 hard cutover).
WS_SLUG="hook-attach-smoke"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - github.com/novshi-tech/boid-kits/hook-attach-smoke
YAML

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "$PROJECT_ID" "$WS_SLUG"

# Restart with a dynamic HTTP address so curl can reach the Web UI.
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

trap '"$E2E_BIN_DIR/boid" stop >/dev/null 2>&1 || true' EXIT

health_json=$(curl -s --unix-socket "$BOID_SOCKET" http://localhost/api/health)
WEB_ADDR=$(printf '%s' "$health_json" | sed -E 's/.*"http_addr":"([^"]+)".*/\1/')
[[ -n "$WEB_ADDR" && "$WEB_ADDR" != "$health_json" ]] || e2e_fail "failed to get web address from health endpoint: $health_json"
e2e_log "web address: $WEB_ADDR"

# Pair + auth so the WS attach endpoint accepts the cookie.
e2e_log "issuing pairing code"
pair_out="$("$E2E_BIN_DIR/boid" web pair)"
code="$(printf '%s\n' "$pair_out" | grep '^Pairing code:' | sed 's/^Pairing code: //')"
[[ -n "$code" ]] || e2e_fail "failed to parse pairing code from: $pair_out"

auth_headers="$(curl -sS -D - -o /dev/null "http://${WEB_ADDR}/auth?token=${code}")"
cookie_val="$(printf '%s\n' "$auth_headers" | grep -i 'set-cookie:' | grep 'boid_session' | head -1 | sed -E 's/.*boid_session=([^;]+).*/\1/')"
[[ -n "$cookie_val" ]] || e2e_fail "failed to extract boid_session cookie"
e2e_log "session cookie obtained (length: ${#cookie_val})"

# Create + start the slow-hook task. The hook prints "hello-hook-pty" and then
# sleeps for several seconds, leaving plenty of time for the WS attach below.
e2e_log "creating task"
task_create_out="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: hook-attach-smoke
title: Hook Attach PTY
behavior: slow
YAML
)"
printf '%s\n' "$task_create_out"
task_id="$(printf '%s\n' "$task_create_out" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting task $task_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

# Wait for the hook job to appear and capture its id + interactive flag.
e2e_log "waiting for hook job to be dispatched"
deadline=$((SECONDS + 10))
job_id=""
while (( SECONDS < deadline )); do
  jobs_resp="$("$E2E_BIN_DIR/boid" job list --task "$task_id" --output json 2>/dev/null || true)"
  # job list returns a JSON array; the first "id": line belongs to the first job.
  job_id="$(printf '%s' "$jobs_resp" | sed -nE 's/.*"id": "([0-9a-f-]+)".*/\1/p' | head -1)"
  if [[ -n "$job_id" ]]; then
    break
  fi
  sleep 0.1
done
[[ -n "$job_id" ]] || e2e_fail "no hook job appeared within 10s (last resp: $jobs_resp)"
e2e_log "hook job_id: $job_id"

# Assert the daemon allocated a PTY for the hook job (Phase 1 regression guard).
e2e_log "verifying hook job has interactive=true and a runtime_id"
job_detail="$("$E2E_BIN_DIR/boid" job show "$job_id" --output json)"
printf '%s\n' "$job_detail"
e2e_assert_contains "$job_detail" '"interactive": true'
e2e_assert_contains "$job_detail" '"tty": true'
e2e_assert_contains "$job_detail" '"runtime_id":'

# Attach over WebSocket and assert the hook's stdout is delivered live.
e2e_log "attaching via WS and waiting for 'hello-hook-pty'"
"$E2E_BIN_DIR/boid-e2e" ws-job-output \
  --addr "http://${WEB_ADDR}" \
  --job "$job_id" \
  --cookie "boid_session=${cookie_val}" \
  --timeout 10s \
  --contains "hello-hook-pty"
e2e_log "OK: 'hello-hook-pty' received via WebSocket PTY attach"

e2e_log "waiting for task to reach done"
"$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 200ms "$task_id" done >/dev/null
e2e_log "hook-attach-smoke scenario completed successfully"
