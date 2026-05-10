#!/usr/bin/env bash
set -euo pipefail

e2e_require_cmd curl

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
PROJECT_ID="e2e-task-behavior-cmd"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

# --- Step 1: create a task with the "plan" behavior (has echo-task-id command) ---
e2e_log "creating task with plan behavior"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: $PROJECT_ID
title: Task Behavior Command Test
behavior: plan
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id from: $task_create_output"
e2e_log "task_id: $task_id"

# --- Step 2: GET /api/tasks/{id}/commands lists echo-task-id ---
e2e_log "checking GET /api/tasks/$task_id/commands"
commands_resp="$(curl -s --unix-socket "$BOID_SOCKET" "http://localhost/api/tasks/$task_id/commands")"
printf '%s\n' "$commands_resp"
e2e_assert_contains "$commands_resp" '"echo-task-id"'
e2e_log "OK: echo-task-id command listed"

# --- Step 3: POST execute ---
e2e_log "executing echo-task-id via POST /api/tasks/$task_id/commands/echo-task-id/execute"
exec_resp="$(curl -sS -w '\n%{http_code}' -X POST \
  --unix-socket "$BOID_SOCKET" \
  "http://localhost/api/tasks/$task_id/commands/echo-task-id/execute")"
http_code="$(printf '%s\n' "$exec_resp" | tail -1)"
body="$(printf '%s\n' "$exec_resp" | head -n -1)"
printf 'http_code=%s body=%s\n' "$http_code" "$body"
[[ "$http_code" == "201" ]] || e2e_fail "expected 201, got $http_code; body: $body"
job_id="$(printf '%s\n' "$body" | sed -E 's/.*"job_id":"([^"]+)".*/\1/')"
[[ -n "$job_id" && "$job_id" != "$body" ]] || e2e_fail "failed to parse job_id from: $body"
e2e_log "job_id: $job_id"

# --- Step 4: wait for job to complete and verify stdout contains task ID ---
e2e_log "waiting for job $job_id to complete"
deadline=$((SECONDS + 20))
while true; do
  job_resp="$(curl -s --unix-socket "$BOID_SOCKET" "http://localhost/api/jobs/$job_id" 2>/dev/null || true)"
  if printf '%s\n' "$job_resp" | grep -q '"completed"'; then
    break
  fi
  if (( SECONDS >= deadline )); then
    e2e_fail "job $job_id did not reach completed status within 20s (last: $job_resp)"
  fi
  sleep 0.2
done
e2e_log "OK: job $job_id completed"

# Output should contain the task ID (echo appends it as argv[1]).
e2e_assert_contains "$job_resp" "$task_id"
e2e_log "OK: job output contains task_id"

# --- Step 5: verify Job.TaskID == task_id ---
job_task_id="$(printf '%s\n' "$job_resp" | sed -E 's/.*"task_id":"([^"]+)".*/\1/')"
[[ "$job_task_id" == "$task_id" ]] || e2e_fail "Job.TaskID=$job_task_id, want $task_id"
e2e_log "OK: Job.TaskID matches task_id"
