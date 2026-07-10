#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
RELEASE_FILE="$PROJECT_DIR/.boid/release-verify-readonly"

rm -f "$RELEASE_FILE"

# Set up workspace for new schema (PR4 hard cutover).
WS_SLUG="readonly-hook-gate"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - readonly-hook-gate
YAML

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "readonly-hook-gate" "$WS_SLUG"

e2e_log "creating readonly task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: readonly-hook-gate
title: Readonly Hook Gate
behavior: supervisor
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting readonly task"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for hook dispatch"
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 1
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 1

e2e_log "releasing hook"
touch "$RELEASE_FILE"

task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'
# task.readonly's semantics changed under the git gateway cutover (PR6):
# readonly now means transport-RO (gateway denies push/fetch-write), not
# filesystem-RO (docs/plans/git-gateway-cutover.md: "readonly の意味論変更:
# FS-RO → transport-RO"). The sandbox-internal clone is always locally
# writable regardless of task.readonly, so the correct expectation here is
# "writable", not the pre-cutover "readonly".
e2e_assert_contains "$task_json" '"fs_status":"writable"'
e2e_assert_contains "$task_json" 'verify-readonly'
