#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "creating gc-smoke task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: gc-smoke
title: GC Smoke Task
behavior: smoke
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting task $task_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "advancing task to done"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type done

e2e_log "waiting for task to reach done"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 10s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'

e2e_log "running gc dry-run (older-than 0s)"
dry_run_output="$("$E2E_BIN_DIR/boid" gc --dry-run --older-than 0s)"
printf '%s\n' "$dry_run_output"
e2e_assert_contains "$dry_run_output" "would delete 1 tasks"

e2e_log "running gc (older-than 0s)"
gc_output="$("$E2E_BIN_DIR/boid" gc --older-than 0s)"
printf '%s\n' "$gc_output"
e2e_assert_contains "$gc_output" "deleted: 1 tasks"

e2e_log "verifying task is removed from list"
task_list="$("$E2E_BIN_DIR/boid" task list)"
printf '%s\n' "$task_list"
e2e_assert_contains "$task_list" "no tasks"
