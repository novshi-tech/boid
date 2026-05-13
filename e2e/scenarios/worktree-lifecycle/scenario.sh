#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "creating worktree-lifecycle task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: worktree-lifecycle
title: Worktree Lifecycle Test
behavior: executor
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting task $task_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for hook job to start (executing state)"
"$E2E_BIN_DIR/boid-e2e" wait-job-count --timeout 15s --interval 100ms "$task_id" 1

e2e_log "verifying git worktree add was invoked"
[[ -f "$E2E_STATE_DIR/fake-git.log" ]] || e2e_fail "missing fake-git.log"
grep -F 'worktree add' "$E2E_STATE_DIR/fake-git.log" >/dev/null || e2e_fail "git worktree add was not invoked"

e2e_log "waiting for task completion"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 30s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'

e2e_log "verifying git worktree remove was invoked"
grep -F 'worktree remove' "$E2E_STATE_DIR/fake-git.log" >/dev/null || e2e_fail "git worktree remove was not invoked"
