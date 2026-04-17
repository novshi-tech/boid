#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

# ============================================================
e2e_log "=== Test 1: artifact.children.all_done — 全子が done で Phase2 が自動 start ==="

phase1_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: phase-dependency
title: Phase1
behavior: smoke
YAML
)"
printf '%s\n' "$phase1_output"
phase1_id="$(printf '%s\n' "$phase1_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$phase1_id" ]] || e2e_fail "failed to parse phase1 id"

e2e_run "$E2E_BIN_DIR/boid" action send --task "$phase1_id" --type start
e2e_run "$E2E_BIN_DIR/boid" action send --task "$phase1_id" --type done

dev1_output="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: phase-dependency
title: Dev1
behavior: smoke
auto_start: true
parent_id: $phase1_id
YAML
)"
printf '%s\n' "$dev1_output"
dev1_id="$(printf '%s\n' "$dev1_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$dev1_id" ]] || e2e_fail "failed to parse dev1 id"

dev2_output="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: phase-dependency
title: Dev2
behavior: smoke
auto_start: true
parent_id: $phase1_id
YAML
)"
printf '%s\n' "$dev2_output"
dev2_id="$(printf '%s\n' "$dev2_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$dev2_id" ]] || e2e_fail "failed to parse dev2 id"

phase2_output="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: phase-dependency
title: Phase2
behavior: smoke
auto_start: true
depends_on:
  - $phase1_id
depends_on_payload: artifact.children.all_done
YAML
)"
printf '%s\n' "$phase2_output"
phase2_id="$(printf '%s\n' "$phase2_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$phase2_id" ]] || e2e_fail "failed to parse phase2 id"

# dev1 dev2 が auto_start で executing になるのを待つ
"$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 10s --interval 100ms "$dev1_id" executing
"$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 10s --interval 100ms "$dev2_id" executing

# Phase2 はまだ pending のはず (全子タスクが done ではない)
phase2_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$phase2_id")"
printf '%s\n' "$phase2_json"
e2e_assert_contains "$phase2_json" '"status":"pending"'

# dev1 を done にする → Phase2 はまだ pending (dev2 が残っている)
e2e_run "$E2E_BIN_DIR/boid" action send --task "$dev1_id" --type done
phase2_after_dev1_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$phase2_id")"
printf '%s\n' "$phase2_after_dev1_json"
e2e_assert_contains "$phase2_after_dev1_json" '"status":"pending"'

# dev2 を done にする → Phase1 の全子が done → Phase2 が自動 start
e2e_run "$E2E_BIN_DIR/boid" action send --task "$dev2_id" --type done
phase2_exec_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 10s --interval 100ms "$phase2_id" executing)"
printf '%s\n' "$phase2_exec_json"
e2e_assert_contains "$phase2_exec_json" '"status":"executing"'

# ============================================================
e2e_log "=== Test 2: artifact.children.all_done — 子が aborted の場合は Phase が auto_start しない ==="

phase3_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: phase-dependency
title: Phase3
behavior: smoke
YAML
)"
printf '%s\n' "$phase3_output"
phase3_id="$(printf '%s\n' "$phase3_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$phase3_id" ]] || e2e_fail "failed to parse phase3 id"

e2e_run "$E2E_BIN_DIR/boid" action send --task "$phase3_id" --type start
e2e_run "$E2E_BIN_DIR/boid" action send --task "$phase3_id" --type done

dev3_output="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: phase-dependency
title: Dev3
behavior: smoke
auto_start: true
parent_id: $phase3_id
YAML
)"
printf '%s\n' "$dev3_output"
dev3_id="$(printf '%s\n' "$dev3_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$dev3_id" ]] || e2e_fail "failed to parse dev3 id"

dev4_output="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: phase-dependency
title: Dev4
behavior: smoke
auto_start: true
parent_id: $phase3_id
YAML
)"
printf '%s\n' "$dev4_output"
dev4_id="$(printf '%s\n' "$dev4_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$dev4_id" ]] || e2e_fail "failed to parse dev4 id"

phase4_output="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: phase-dependency
title: Phase4
behavior: smoke
auto_start: true
depends_on:
  - $phase3_id
depends_on_payload: artifact.children.all_done
YAML
)"
printf '%s\n' "$phase4_output"
phase4_id="$(printf '%s\n' "$phase4_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$phase4_id" ]] || e2e_fail "failed to parse phase4 id"

# dev3 dev4 が auto_start で executing になるのを待つ
"$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 10s --interval 100ms "$dev3_id" executing
"$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 10s --interval 100ms "$dev4_id" executing

# dev3 done, dev4 abort → all_done は false のまま
e2e_run "$E2E_BIN_DIR/boid" action send --task "$dev3_id" --type done
e2e_run "$E2E_BIN_DIR/boid" action send --task "$dev4_id" --type abort

"$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 10s --interval 100ms "$dev4_id" aborted

# Phase4 は pending のまま (aborted child が存在するため all_done は false)
phase4_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$phase4_id")"
printf '%s\n' "$phase4_json"
e2e_assert_contains "$phase4_json" '"status":"pending"'
