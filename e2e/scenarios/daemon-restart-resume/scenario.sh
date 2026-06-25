#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
RELEASE_FILE="$PROJECT_DIR/.boid/release-block-and-done"

rm -f "$RELEASE_FILE"

# Set up workspace for new schema (PR4 hard cutover).
WS_SLUG="daemon-restart-resume"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - daemon-restart-resume
YAML

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "daemon-restart-resume" "$WS_SLUG"

e2e_log "creating task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: daemon-restart-resume
title: Daemon Restart Resume
behavior: supervisor
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting task"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for hook to be in flight"
"$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 15s --interval 100ms "$task_id" executing >/dev/null
"$E2E_BIN_DIR/boid-e2e" wait-job-count --timeout 15s --interval 100ms "$task_id" 1

e2e_log "stopping daemon while hook is blocked"
"$E2E_BIN_DIR/boid" stop || true

# Wait for the unix socket to disappear (daemon fully gone).
for _ in $(seq 1 100); do
  [[ -S "$BOID_SOCKET" ]] || break
  sleep 0.1
done
[[ ! -S "$BOID_SOCKET" ]] || e2e_fail "daemon socket still present after stop"

e2e_log "starting daemon again"
"$E2E_BIN_DIR/boid" start \
  --db-path "$XDG_DATA_HOME/boid/boid.db" \
  --socket-path "$BOID_SOCKET" \
  --kits-dir "$XDG_DATA_HOME/boid/kits" \
  --key-file-path "$XDG_DATA_HOME/boid/boid-secret.key" \
  >>"$E2E_LOG_DIR/server.stdout.log" \
  2>>"$E2E_LOG_DIR/server.stderr.log"

e2e_run "$E2E_BIN_DIR/boid-e2e" wait-health --timeout 15s --interval 100ms "$BOID_SOCKET"

e2e_log "verifying task auto-reopened back to executing"
resumed_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 15s --interval 100ms "$task_id" executing)"
printf '%s\n' "$resumed_json"
e2e_assert_contains "$resumed_json" '"status":"executing"'

# The action history must show daemon_shutdown abort followed by an automatic
# reopen — that is the contract the startup auto-reopen path is meant to provide.
task_show="$("$E2E_BIN_DIR/boid" task show "$task_id")"
printf '%s\n' "$task_show"
e2e_assert_contains "$task_show" 'code: daemon_shutdown'
e2e_assert_contains "$task_show" 'reopen'

e2e_log "releasing hook so the resumed task can complete"
touch "$RELEASE_FILE"

e2e_log "waiting for task to reach done"
done_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 30s --interval 200ms "$task_id" done)"
printf '%s\n' "$done_json"
e2e_assert_contains "$done_json" '"status":"done"'
# resumed=true is set by the hook script — proves the hook was re-dispatched
# after the auto-reopen rather than the prior failed job being credited.
e2e_assert_contains "$done_json" '"resumed":true'
