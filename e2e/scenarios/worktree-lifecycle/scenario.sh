#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "creating worktree task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: worktree-lifecycle
title: Worktree Lifecycle
behavior: dev
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting worktree task $task_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

# git worktree add is called synchronously inside PlanHook before the job is
# created in the DB, so once wait-job-count returns the git log already has it.
e2e_log "waiting for hook job to be dispatched"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 1

e2e_log "verifying git worktree add was called"
e2e_wait_for_file "$E2E_STATE_DIR/fake-git.log"
git_log="$(cat "$E2E_STATE_DIR/fake-git.log")"
e2e_assert_contains "$git_log" "worktree add"

e2e_log "waiting for task done"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 30s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'

e2e_log "verifying git worktree remove was called"
git_log="$(cat "$E2E_STATE_DIR/fake-git.log")"
e2e_assert_contains "$git_log" "worktree remove"
