#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "listing scripts for project"
script_list="$("$E2E_BIN_DIR/boid" script list --project e2e-script-manual)"
printf '%s\n' "$script_list"
e2e_assert_contains "$script_list" "echo-task"
e2e_assert_contains "$script_list" "on-done"

e2e_log "running echo-task script manually"
run_output="$("$E2E_BIN_DIR/boid" script run --project e2e-script-manual e2e-scripts/echo-task)"
printf '%s\n' "$run_output"
task_id="$(printf '%s\n' "$run_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "waiting for ephemeral task $task_id to reach done"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"ephemeral":true'
e2e_assert_contains "$task_json" '"status":"done"'
