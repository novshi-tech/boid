#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
PROJECT_ID="e2e-script-smoke"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "listing scripts for project"
script_list="$("$E2E_BIN_DIR/boid" script list --project "$PROJECT_ID")"
printf '%s\n' "$script_list"
e2e_assert_contains "$script_list" "ping"
e2e_assert_contains "$script_list" "Simple ping script"

e2e_log "running a script manually"
run_out="$("$E2E_BIN_DIR/boid" script run script-smoke/ping --project "$PROJECT_ID")"
printf '%s\n' "$run_out"
e2e_assert_contains "$run_out" "task created:"
