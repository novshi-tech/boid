#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "listing registered projects"
project_list="$("$E2E_BIN_DIR/boid" project list)"
printf '%s\n' "$project_list"
e2e_assert_contains "$project_list" "e2e-smoke"
