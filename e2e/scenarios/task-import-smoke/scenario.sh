#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
TASKS_JSONL="$scenario_dir/fixtures/tasks.jsonl"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "importing tasks from JSONL"
import_output="$("$E2E_BIN_DIR/boid" task import -f "$TASKS_JSONL")"
printf '%s\n' "$import_output"
e2e_assert_contains "$import_output" "Created: 3"

e2e_log "listing tasks"
task_list="$("$E2E_BIN_DIR/boid" task list)"
printf '%s\n' "$task_list"
e2e_assert_contains "$task_list" "Task A"
e2e_assert_contains "$task_list" "Task B"
e2e_assert_contains "$task_list" "Task C"

e2e_log "re-importing same file (expecting skips)"
reimport_output="$("$E2E_BIN_DIR/boid" task import -f "$TASKS_JSONL")"
printf '%s\n' "$reimport_output"
e2e_assert_contains "$reimport_output" "Skipped: 3"

e2e_log "importing with --project flag (no project_id in JSONL)"
TASKS_NOPROJECT="$E2E_ROOT/tasks-noproject.jsonl"
printf '{"title":"Task D","behavior":"smoke","remote_id":"RD","datasource_id":"e2e"}\n' > "$TASKS_NOPROJECT"
flag_output="$("$E2E_BIN_DIR/boid" task import -f "$TASKS_NOPROJECT" --project e2e-import)"
printf '%s\n' "$flag_output"
e2e_assert_contains "$flag_output" "Created: 1"

e2e_log "verifying Task D exists in task list"
task_list2="$("$E2E_BIN_DIR/boid" task list)"
printf '%s\n' "$task_list2"
e2e_assert_contains "$task_list2" "Task D"
