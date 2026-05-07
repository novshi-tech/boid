#!/usr/bin/env bash
set -euo pipefail

# Scenario: Q&A サイクルの end-to-end 検証
#
# 1. タスクを作成・開始 → フック (fake-agent) が notify --ask を呼ぶ → awaiting
# 2. awaiting payload に session_id / question / question_id が入っていることを確認
# 3. boid task answer --answer "approve" → フックが 2 回目起動 → done
# 4. 2 回目起動時の env vars (BOID_AGENT_SESSION_ID, BOID_USER_ANSWER) を確認
# 5. (reject パス) 別タスクで --answer "reject" → aborted

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

# ── approve パス ──────────────────────────────────────────────────────────────

e2e_log "creating Q&A task (approve path)"
task_create_out="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: q-and-a
title: Q&A Cycle Test (approve)
behavior: qa
YAML
)"
printf '%s\n' "$task_create_out"
task_id="$(printf '%s\n' "$task_create_out" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting task $task_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for task to reach awaiting (hook called notify --ask)"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" awaiting)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"awaiting"'

e2e_log "verifying awaiting payload contains session_id, question, question_id"
e2e_assert_contains "$task_json" '"session_id":"fake-session-xyz"'
e2e_assert_contains "$task_json" '"question":"Approve?"'
e2e_assert_contains "$task_json" '"question_id":"q-approve-001"'

e2e_log "submitting answer: approve"
e2e_run "$E2E_BIN_DIR/boid" task answer \
    --task "$task_id" \
    --question-id "q-approve-001" \
    --answer "approve"

e2e_log "waiting for task to complete (done)"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'

e2e_log "verifying artifact was written by 2nd hook invocation"
e2e_assert_contains "$task_json" '"status":"done"'

e2e_log "verifying env vars were passed to 2nd hook invocation"
log="$E2E_STATE_DIR/fake-agent.log"
[[ -f "$log" ]] || e2e_fail "missing fake-agent.log (hook did not run?)"
grep -F 'BOID_AGENT_SESSION_ID=fake-session-xyz' "$log" >/dev/null \
    || e2e_fail "BOID_AGENT_SESSION_ID was not set on 2nd invocation"
grep -F 'BOID_USER_ANSWER=approve' "$log" >/dev/null \
    || e2e_fail "BOID_USER_ANSWER was not set on 2nd invocation"
grep -F 'BOID_QUESTION_ID=q-approve-001' "$log" >/dev/null \
    || e2e_fail "BOID_QUESTION_ID was not set on 2nd invocation"

e2e_log "approve path passed"

# ── reject パス ───────────────────────────────────────────────────────────────

e2e_log "creating Q&A task (reject path)"
# Reset log to separate reject-path entries.
rm -f "$log"
task2_create_out="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: q-and-a
title: Q&A Cycle Test (reject)
behavior: qa
YAML
)"
printf '%s\n' "$task2_create_out"
task2_id="$(printf '%s\n' "$task2_create_out" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task2_id" ]] || e2e_fail "failed to parse task2 id"

e2e_log "starting task $task2_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task2_id" --type start

e2e_log "waiting for task2 to reach awaiting"
"$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task2_id" awaiting >/dev/null

e2e_log "submitting answer: reject"
e2e_run "$E2E_BIN_DIR/boid" task answer \
    --task "$task2_id" \
    --question-id "q-approve-001" \
    --answer "reject"

e2e_log "waiting for task2 to be aborted (2nd hook exits non-zero)"
task2_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task2_id" aborted)"
printf '%s\n' "$task2_json"
e2e_assert_contains "$task2_json" '"status":"aborted"'

e2e_log "reject path passed"
e2e_log "Q&A cycle scenario completed successfully"
