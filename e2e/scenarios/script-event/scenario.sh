#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "creating simple task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: e2e-script-event
title: Simple Task
behavior: simple
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting simple task $task_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for simple task to reach done"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'

e2e_log "waiting for on-done script task to complete"
on_done_task_id=""
deadline=$((SECONDS + 30))
while [[ -z "$on_done_task_id" ]]; do
    (( SECONDS < deadline )) || e2e_fail "timed out waiting for on-done script task"
    task_list="$("$E2E_BIN_DIR/boid" task list --status done)"
    on_done_task_id="$(printf '%s\n' "$task_list" | awk '/script: e2e-scripts\/on-done/{print $1; exit}')"
    [[ -z "$on_done_task_id" ]] && sleep 0.1
done

e2e_log "verifying on-done script task payload contains trigger event"
on_done_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$on_done_task_id")"
printf '%s\n' "$on_done_json"
e2e_assert_contains "$on_done_json" '"task_done"'
e2e_assert_contains "$on_done_json" '"ephemeral":true'
