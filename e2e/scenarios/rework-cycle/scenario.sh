#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
RELEASE_HOOK="$PROJECT_DIR/.boid/release-rework-hook"

rm -f "$RELEASE_HOOK"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "creating abort task"
abort_task_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: rework-cycle
title: Manual Abort
behavior: feedback
YAML
)"
printf '%s\n' "$abort_task_output"
abort_task_id="$(printf '%s\n' "$abort_task_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$abort_task_id" ]] || e2e_fail "failed to parse abort task id"

e2e_log "starting abort task"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$abort_task_id" --type start
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$abort_task_id" 1

e2e_log "manually aborting task"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$abort_task_id" --type abort
abort_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 10s --interval 100ms "$abort_task_id" aborted)"
printf '%s\n' "$abort_json"
e2e_assert_contains "$abort_json" '"status":"aborted"'
touch "$RELEASE_HOOK"

e2e_log "creating rework task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: rework-cycle
title: Verification Rework
behavior: feedback
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting rework task"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for executing hook"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 1
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 1
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" gate 0

e2e_log "releasing hook"
touch "$RELEASE_HOOK"

e2e_log "waiting for verification gate"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 2
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 1
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" gate 1

e2e_log "waiting for rework cycle"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 3

task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" reworking)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"reworking"'
e2e_assert_contains "$task_json" '"source_state":"verifying"'
e2e_assert_contains "$task_json" '"needs rework"'
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 2
