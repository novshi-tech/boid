#!/usr/bin/env bash
set -euo pipefail

# Scenario: hook calls `boid job done` explicitly before exiting, then sleeps.
# The daemon must (1) transition the task to done, (2) send SIGTERM to the hook
# process, and (3) absorb the second `boid job done` fired by the EXIT trap
# (which carries exit-code 143 after SIGTERM). Without idempotency the job
# status would flip from "completed" to "failed".

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

# Set up workspace for new schema (PR4 hard cutover).
WS_SLUG="job-done-explicit"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - github.com/novshi-tech/boid-kits/job-done-explicit
YAML

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "job-done-explicit" "$WS_SLUG"

e2e_log "creating job-done-explicit task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: job-done-explicit
title: Job Done Explicit Test
behavior: explicit
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting task $task_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for hook job to appear"
"$E2E_BIN_DIR/boid-e2e" wait-job-count --timeout 15s --interval 100ms "$task_id" 1

e2e_log "waiting for task to reach done (hook calls boid job done explicitly)"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'

e2e_log "verifying hook job ended as completed (not failed)"
jobs_json="$("$E2E_BIN_DIR/boid-e2e" list-jobs "$task_id")"
printf '%s\n' "$jobs_json"
e2e_assert_contains "$jobs_json" '"status":"completed"'
