#!/usr/bin/env bash
set -euo pipefail

# Scenario: blocking Q&A (boid task ask) end-to-end verification.
#
# Verifies the harness-independent blocking Q&A RPC (`boid task ask`, merged as
# PR #609). The legacy session-resume path (`notify --ask` + `reopen`) was
# removed in the session-id-resume cleanup: the agent stays alive and blocks
# inside a single hook job until the answer arrives over the held broker
# connection — there is no longer a 2nd resume dispatch to test.
#
# Flow:
#   1. Task starts -> fake-agent hook calls `boid task ask` and BLOCKS -> awaiting
#   2. awaiting payload carries the question text (proves the ask transition
#      fired). The legacy mode discriminator was removed alongside session-id
#      resume: every awaiting record is now blocking by construction.
#   3. host submits `boid task answer` -> the parked agent unblocks WITHOUT a
#      resume dispatch (hook job count stays 1) and receives the answer on stdout
#   4. agent records the answer in the artifact and exits 0 -> done

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
ANSWER_TEXT="xyzzy"

# Set up workspace for new schema (PR4 hard cutover).
WS_SLUG="task-ask-blocking"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - task-ask-blocking-smoke
YAML

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "task-ask-blocking" "$WS_SLUG"

e2e_log "creating blocking Q&A task"
task_create_out="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: task-ask-blocking
title: Blocking Q&A Test
behavior: qa
YAML
)"
printf '%s\n' "$task_create_out"
task_id="$(printf '%s\n' "$task_create_out" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting task $task_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for task to reach awaiting (agent blocked inside boid task ask)"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" awaiting)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"awaiting"'

e2e_log "verifying awaiting payload carries the question"
e2e_assert_contains "$task_json" '"question":"What is the magic word?"'

e2e_log "verifying exactly one hook job (blocking holds the same job; no resume dispatch)"
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 1

# The daemon generates the question_id (newQuestionID); read it back from the
# awaiting payload rather than hard-coding it.
qid="$(printf '%s\n' "$task_json" | sed -n 's/.*"question_id":"\([0-9a-f-]*\)".*/\1/p')"
[[ -n "$qid" ]] || e2e_fail "failed to parse question_id from awaiting payload"

e2e_log "submitting answer via boid task answer (question_id=$qid)"
e2e_run "$E2E_BIN_DIR/boid" task answer \
    --task "$task_id" \
    --question-id "$qid" \
    --answer "$ANSWER_TEXT"

e2e_log "waiting for task to complete (agent unblocked, received answer, exited 0)"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'

e2e_log "verifying the agent received the answer over the blocking RPC"
e2e_assert_contains "$task_json" "\"received_answer\":\"$ANSWER_TEXT\""

e2e_log "verifying still exactly one hook job (answer did not trigger a resume dispatch)"
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 1

e2e_log "blocking Q&A scenario completed successfully"
