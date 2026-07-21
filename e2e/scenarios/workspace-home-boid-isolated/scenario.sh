#!/usr/bin/env bash
set -euo pipefail

# docs/plans/home-workspace-volume.md Phase 4 PR6, scenario B: pins that
# $HOME/.boid does NOT persist across jobs, unlike the rest of $HOME
# (scenario A, workspace-home-persistence, pins the opposite for the rest of
# $HOME). A true concurrent-dispatch version of this scenario is
# significantly harder to pin reliably in this harness (synchronizing two
# independent task/job lifecycles through boid's CLI without a race), and
# the plan doc explicitly sanctions this sequential fallback: job 1 writes a
# marker under $HOME/.boid, job 2 (a separate job dispatched afterward, same
# workspace) must observe it gone — proving $HOME/.boid is torn down and
# freshly remounted on every dispatch regardless of dispatch ordering, which
# is a strictly stronger guarantee than "concurrent jobs don't race" would
# be (a job-scoped tmpfs that only avoided collisions by lucky scheduling
# would still fail this test).
#
# See docs/plans/phase5-shim-and-task-context.md 「PR 分割案 > 5b」6 for why
# this overlay (and therefore this scenario) survived the Phase 5b PR6
# cutover intact — an early cut of that PR retired it, and codex review
# (Blocker + Major, before merge) found the resulting shared, persistent
# $HOME/.boid/output/payload_patch.json path exploitable across concurrent
# jobs and via ancestor-symlink redirection; both classes of attack are
# structurally impossible once $HOME/.boid is back to being its own private,
# job-scoped tmpfs mount (a fresh mount in a fresh mount namespace per job
# has no shared backing path for another job's sandbox to ever observe or
# redirect, regardless of scheduling — see
# TestHomeMounts_WorkspaceHomeDirSet_ReturnsBindPlusBoidTmpfs and
# internal/dispatcher/sandbox_builder.go's homeMounts doc comment for the
# unit-level pin of that property).

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
WS_SLUG="pr6-boid-isolated"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "creating workspace $WS_SLUG"
e2e_run "$E2E_BIN_DIR/boid" workspace create "$WS_SLUG"

e2e_log "assigning project to $WS_SLUG"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "e2e-home-boid-isolated" "$WS_SLUG"

e2e_log "=== job 1: write to \$HOME/.boid in $WS_SLUG ==="
task1_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: e2e-home-boid-isolated
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

e2e_log "=== job 2: read \$HOME/.boid in $WS_SLUG (must NOT see job 1's write) ==="
task2_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: e2e-home-boid-isolated
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
e2e_assert_contains "$task2_json" '"read":"EMPTY"'
