#!/usr/bin/env bash
set -euo pipefail

# docs/plans/phase5-shim-and-task-context.md 「PR 分割案 > 5b」6 (Phase 5b PR6
# cutover, decision 7): this scenario used to be workspace-home-boid-isolated
# and pinned that $HOME/.boid stayed a job-scoped tmpfs even though the rest
# of $HOME persists (docs/plans/home-workspace-volume.md Phase 4 PR6). That
# tmpfs overlay existed only to isolate dispatch-time context/output file
# writes ($HOME/.boid/{context,output}/*) between jobs sharing a workspace
# home; now that contextFiles/the attachments RO bind are gone, PR6 retires
# the overlay too — $HOME/.boid persists across jobs exactly like the rest of
# $HOME (see workspace-home-persistence, "scenario A", for the $HOME-proper
# version of this same assertion).
#
# The narrow exception is $HOME/.boid/output/payload_patch.json: a defensive,
# job-start cleanup (sandbox.Spec.RemoveFiles) still removes any leftover
# copy so a job that writes no patch of its own never silently inherits a
# previous job's stale one. Job 2's hook checks for that file's presence
# *before* writing its own to pin that the cleanup ran ahead of it.

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
WS_SLUG="pr6-boid-persists"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "creating workspace $WS_SLUG"
e2e_run "$E2E_BIN_DIR/boid" workspace create "$WS_SLUG"

e2e_log "assigning project to $WS_SLUG"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "e2e-home-boid-persists" "$WS_SLUG"

e2e_log "=== job 1: write to \$HOME/.boid in $WS_SLUG ==="
task1_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: e2e-home-boid-persists
title: Write Boid
behavior: write-boid
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

e2e_log "=== job 2: read \$HOME/.boid in $WS_SLUG (must see job 1's marker, must NOT see job 1's stale payload_patch.json) ==="
task2_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: e2e-home-boid-persists
title: Read Boid
behavior: read-boid
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
e2e_assert_contains "$task2_json" '"marker":"job1"'
e2e_assert_contains "$task2_json" '"stale_patch":"ABSENT"'
