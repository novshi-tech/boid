#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
RELEASE_HOOK="$PROJECT_DIR/.boid/release-feedback-hook"

rm -f "$RELEASE_HOOK"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "creating feedback-loop task"
task_create_output="$("$E2E_BIN_DIR/boid" task create --title "Feedback Loop Full" --project feedback-loop-full --behavior feedback)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting feedback-loop task"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "releasing first executing hook"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 1
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 1
touch "$RELEASE_HOOK"

e2e_log "waiting for verifying gate"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 2
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" gate 1

task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" in_review)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"in_review"'

e2e_log "collecting feedback and forcing rework"
rm -f "$RELEASE_HOOK"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type collect_feedback
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 3

task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" executing)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"executing"'
e2e_assert_contains "$task_json" '"source_state":"collecting_feedback"'
e2e_assert_contains "$task_json" '"needs more work"'

e2e_log "running second execution cycle"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 4
touch "$RELEASE_HOOK"

e2e_log "running second verification cycle"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 5

task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" in_review)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"in_review"'

e2e_log "collecting final feedback"
rm -f "$RELEASE_HOOK"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type collect_feedback
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 6

task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'
e2e_assert_contains "$task_json" '"feedback complete"'
