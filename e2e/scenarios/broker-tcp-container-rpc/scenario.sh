#!/usr/bin/env bash
set -euo pipefail

# Scenario: a `dev` task's hook calls `boid task update --payload-patch @-`
# — the broker RPC path docs/plans/phase6-cutover-followups.md §⓪ closes
# for the container backend. Run under ./e2e/run.sh (the userns-backend
# daemon this harness always drives), this pins the UNIX-transport branch
# of the refactored internal/sandbox/brokerclient.SendJSONFromEnv/JobDone
# decision point — the TLS-transport branch (a real container-backend
# sibling container) has no equivalent here; see
# e2e/run-container.sh's own broker RPC check for that half.

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
WS_SLUG="broker-tcp-container-rpc"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<'YAML'
{}
YAML

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "broker-tcp-container-rpc" "$WS_SLUG"

e2e_log "creating dev task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: broker-tcp-container-rpc
title: Broker RPC round trip
behavior: dev
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting task $task_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for task to reach done"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'
e2e_assert_contains "$task_json" '"result":"pass"'
e2e_assert_contains "$task_json" '"broker_transport":"unix"'
