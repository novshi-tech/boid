#!/usr/bin/env bash
set -euo pipefail

# docs/plans/home-workspace-volume.md Phase 4 PR6, scenario A: pins that the
# same workspace's $HOME persists across two separate job dispatches (a
# host-side per-workspace bind mount now, not a fresh tmpfs per job as in the
# pre-Phase-4 world). Job 1's hook writes a file under $HOME; job 2's hook —
# a completely separate task/job — reads it back and must see job 1's
# content.

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
WS_SLUG="pr6-persistence"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "creating workspace $WS_SLUG"
e2e_run "$E2E_BIN_DIR/boid" workspace create "$WS_SLUG"

e2e_log "assigning project to $WS_SLUG"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "e2e-home-persistence" "$WS_SLUG"

e2e_log "=== job 1: write to \$HOME in $WS_SLUG ==="
task1_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: e2e-home-persistence
title: Write Home
behavior: write-home
YAML
)"
printf '%s\n' "$task1_output"
task1_id="$(printf '%s\n' "$task1_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task1_id" ]] || e2e_fail "failed to parse task1 id"

e2e_run "$E2E_BIN_DIR/boid" action send --task "$task1_id" --type start

e2e_log "waiting for job 1 to complete"
task1_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 30s --interval 200ms "$task1_id" done)"
printf '%s\n' "$task1_json"
e2e_assert_contains "$task1_json" '"status":"done"'

e2e_log "=== job 2: read \$HOME in $WS_SLUG (should see job 1's write) ==="
task2_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: e2e-home-persistence
title: Read Home
behavior: read-home
YAML
)"
printf '%s\n' "$task2_output"
task2_id="$(printf '%s\n' "$task2_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task2_id" ]] || e2e_fail "failed to parse task2 id"

e2e_run "$E2E_BIN_DIR/boid" action send --task "$task2_id" --type start

e2e_log "waiting for job 2 to complete"
task2_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 30s --interval 200ms "$task2_id" done)"
printf '%s\n' "$task2_json"
e2e_assert_contains "$task2_json" '"status":"done"'
e2e_assert_contains "$task2_json" '"persisted":"job1-data"'
