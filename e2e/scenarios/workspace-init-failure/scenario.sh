#!/usr/bin/env bash
set -euo pipefail

# docs/plans/home-workspace-volume.md Phase 4 PR6, scenario D: pins that a
# failing workspace init.sh makes dispatch fail explicitly (internal/
# dispatcher/runner.go's Runner.Dispatch: resolveWorkspaceHome's error goes
# straight to failJob + cleanup + an error return, exactly like every other
# pre-BuildSandboxSpec dispatch error — see the Phase 4 PR1 wiring guard
# tests in internal/dispatcher/workspace_home_dispatch_test.go for the unit-
# level pin of that same contract).
#
# The hook never actually runs: resolveWorkspaceHome fails before
# BuildSandboxSpec is ever called, so the job row created by CreateJob is
# marked failed with the init script's error (exit code + output tail)
# without a sandbox ever launching. The failure surfaces asynchronously
# (TaskWorkflowService.runDispatchLoop runs DispatchAndAdvance in a
# background goroutine — action send itself returns success immediately),
# transitioning the task straight to aborted via abortOnDispatchError.

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
WS_SLUG="pr6-init-fail"
INIT_SCRIPT_DIR="$XDG_CONFIG_HOME/boid/workspaces/$WS_SLUG"

e2e_log "planting an always-failing init.sh for $WS_SLUG"
mkdir -p "$INIT_SCRIPT_DIR"
cat > "$INIT_SCRIPT_DIR/init.sh" <<'EOF'
#!/bin/bash
echo "boom: this workspace's init.sh always fails" >&2
exit 1
EOF

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "creating workspace $WS_SLUG"
e2e_run "$E2E_BIN_DIR/boid" workspace create "$WS_SLUG"

e2e_log "assigning project to $WS_SLUG"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "e2e-init-fail" "$WS_SLUG"

task_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: e2e-init-fail
title: Ping With Failing Init
behavior: ping
YAML
)"
printf '%s\n' "$task_output"
task_id="$(printf '%s\n' "$task_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting task $task_id (workspace init.sh always fails)"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for the task to abort due to the init script failure"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 200ms "$task_id" aborted)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"aborted"'

e2e_log "verifying the pre-sandbox job was marked failed with the init script's error"
jobs_json="$("$E2E_BIN_DIR/boid-e2e" list-jobs "$task_id")"
printf '%s\n' "$jobs_json"
e2e_assert_contains "$jobs_json" '"status":"failed"'
e2e_assert_contains "$jobs_json" "init script"
e2e_assert_contains "$jobs_json" "exited 1"
