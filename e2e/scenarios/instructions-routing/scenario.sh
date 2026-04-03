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

e2e_log "starting task"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for executing hook (agent-a)"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 1
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 1

e2e_log "waiting for agent-a state file"
e2e_wait_for_file "$E2E_STATE_DIR/agent-a-instructions.json"

e2e_log "verifying agent-a BOID_INSTRUCTIONS"
agent_a_instructions="$(cat "$E2E_STATE_DIR/agent-a-instructions.json")"
printf '%s\n' "$agent_a_instructions"
e2e_assert_contains "$agent_a_instructions" '"role":"executor"'
e2e_assert_contains "$agent_a_instructions" '"type":"execution"'
e2e_assert_contains "$agent_a_instructions" '"consumer":"agent-a"'
e2e_assert_contains "$agent_a_instructions" '"message":"implement the task"'

e2e_log "waiting for task to reach verifying (agent-a hook completes, artifact set)"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" verifying)"
e2e_assert_contains "$task_json" '"status":"verifying"'

e2e_log "waiting for verifying hook (agent-b)"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 2
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 2

e2e_log "waiting for agent-b state file"
e2e_wait_for_file "$E2E_STATE_DIR/agent-b-instructions.json"

e2e_log "verifying agent-b BOID_INSTRUCTIONS (2 instructions, role-sorted)"
agent_b_instructions="$(cat "$E2E_STATE_DIR/agent-b-instructions.json")"
printf '%s\n' "$agent_b_instructions"
# 2件の instructions が含まれること
e2e_assert_contains "$agent_b_instructions" '"role":"reviewer"'
e2e_assert_contains "$agent_b_instructions" '"role":"security"'
e2e_assert_contains "$agent_b_instructions" '"type":"verification"'
e2e_assert_contains "$agent_b_instructions" '"consumer":"agent-b"'
# reviewer の message が request payload で上書きされていること
e2e_assert_contains "$agent_b_instructions" '"message":"review correctness (overridden)"'
# security の message は default_payload のまま
e2e_assert_contains "$agent_b_instructions" '"message":"check security aspects"'
# role 昇順: reviewer < security（配列の順序を確認）
reviewer_pos="${agent_b_instructions%%\"role\":\"reviewer\"*}"
security_pos="${agent_b_instructions%%\"role\":\"security\"*}"
[[ ${#reviewer_pos} -lt ${#security_pos} ]] || e2e_fail "expected reviewer before security in BOID_INSTRUCTIONS"

e2e_log "waiting for in_review (resolved verification)"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" in_review)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"in_review"'
e2e_assert_contains "$task_json" '"artifact"'
e2e_assert_contains "$task_json" '"verification"'

e2e_log "instructions-routing scenario completed"
