#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

# --- default_payload + request payload merge テスト ---
# reviewer の message を request payload で上書きする
e2e_log "creating task with payload override (reviewer message)"
task_create_output="$(
  "$E2E_BIN_DIR/boid" task create \
    --title "Instructions Routing Test" \
    --project instructions-routing \
    --behavior impl \
    --payload '{"instructions":{"reviewer":{"type":"verification","consumer":"agent-b","message":"review correctness (overridden)"}}}'
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

# タスク作成直後にペイロードの instructions merge を確認する
e2e_log "verifying merged payload instructions"
task_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$task_id")"
e2e_assert_contains "$task_json" '"executor"'
e2e_assert_contains "$task_json" '"reviewer"'
e2e_assert_contains "$task_json" '"security"'
# reviewer の message が上書きされていること
e2e_assert_contains "$task_json" '"message":"review correctness (overridden)"'
# security の message は default_payload のまま
e2e_assert_contains "$task_json" '"message":"check security aspects"'

e2e_log "starting task"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for executing hook (agent-a)"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 1
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 1

# executing フェーズは read-write のため state ファイルに BOID_INSTRUCTIONS を記録できる
e2e_log "waiting for agent-a state file"
e2e_wait_for_file "$PROJECT_DIR/agent-a-instructions.json"

e2e_log "verifying agent-a BOID_INSTRUCTIONS (1 instruction: execution type)"
agent_a_instructions="$(cat "$PROJECT_DIR/agent-a-instructions.json")"
printf '%s\n' "$agent_a_instructions"
e2e_assert_contains "$agent_a_instructions" '"role":"executor"'
e2e_assert_contains "$agent_a_instructions" '"type":"execution"'
e2e_assert_contains "$agent_a_instructions" '"consumer":"agent-a"'
e2e_assert_contains "$agent_a_instructions" '"message":"implement the task"'
# verification type の instructions は含まれないこと
if printf '%s' "$agent_a_instructions" | grep -q '"type":"verification"'; then
    e2e_fail "agent-a should not receive verification type instructions"
fi

e2e_log "waiting for task to reach verifying (artifact set → auto-advance)"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" verifying)"
e2e_assert_contains "$task_json" '"status":"verifying"'

e2e_log "waiting for verifying hook (agent-b fires with verification type instructions)"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 2
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 2

e2e_log "waiting for in_review (agent-b emits resolved verification)"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" in_review)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"in_review"'
e2e_assert_contains "$task_json" '"artifact"'
e2e_assert_contains "$task_json" '"verification"'

e2e_log "instructions-routing scenario completed"
