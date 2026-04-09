#!/usr/bin/env bash
set -euo pipefail

ALPHA_DIR="$E2E_WORKSPACE_DIR/alpha"
BETA_DIR="$E2E_WORKSPACE_DIR/beta"

e2e_log "registering project-alpha"
e2e_run "$E2E_BIN_DIR/boid" project add "$ALPHA_DIR"

e2e_log "registering project-beta"
e2e_run "$E2E_BIN_DIR/boid" project add "$BETA_DIR"

e2e_log "listing registered projects"
project_list="$("$E2E_BIN_DIR/boid" project list)"
printf '%s\n' "$project_list"
e2e_assert_contains "$project_list" "project-alpha"
e2e_assert_contains "$project_list" "project-beta"

e2e_log "creating task in project-alpha"
alpha_task_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: project-alpha
title: Alpha Task
behavior: alpha
YAML
)"
printf '%s\n' "$alpha_task_output"
alpha_task_id="$(printf '%s\n' "$alpha_task_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$alpha_task_id" ]] || e2e_fail "failed to parse alpha task id"

e2e_log "creating task in project-beta"
beta_task_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: project-beta
title: Beta Task
behavior: beta
YAML
)"
printf '%s\n' "$beta_task_output"
beta_task_id="$(printf '%s\n' "$beta_task_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$beta_task_id" ]] || e2e_fail "failed to parse beta task id"

e2e_log "verifying alpha task is associated with project-alpha"
alpha_task_detail="$("$E2E_BIN_DIR/boid" task show "$alpha_task_id")"
printf '%s\n' "$alpha_task_detail"
e2e_assert_contains "$alpha_task_detail" "project-alpha"

e2e_log "verifying beta task is associated with project-beta"
beta_task_detail="$("$E2E_BIN_DIR/boid" task show "$beta_task_id")"
printf '%s\n' "$beta_task_detail"
e2e_assert_contains "$beta_task_detail" "project-beta"

e2e_log "removing project-alpha"
e2e_run "$E2E_BIN_DIR/boid" project remove project-alpha

e2e_log "verifying project-beta task is unaffected after project-alpha removal"
beta_task_detail_after="$("$E2E_BIN_DIR/boid" task show "$beta_task_id")"
printf '%s\n' "$beta_task_detail_after"
e2e_assert_contains "$beta_task_detail_after" "project-beta"
