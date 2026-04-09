#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

# ============================================================
e2e_log "=== Test 1: auto_start 基本動作 ==="

task1_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: auto-start-deps
title: Auto Start Task
behavior: smoke
auto_start: true
YAML
)"
printf '%s\n' "$task1_output"
task1_id="$(printf '%s\n' "$task1_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task1_id" ]] || e2e_fail "failed to parse task1 id"

task1_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$task1_id")"
printf '%s\n' "$task1_json"
e2e_assert_contains "$task1_json" '"status":"executing"'

# ============================================================
e2e_log "=== Test 2: 依存関係がブロックする ==="

dep2_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: auto-start-deps
title: Dep Task 2 (pending)
behavior: smoke
YAML
)"
printf '%s\n' "$dep2_output"
dep2_id="$(printf '%s\n' "$dep2_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$dep2_id" ]] || e2e_fail "failed to parse dep2 id"

child2_output="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: auto-start-deps
title: Child Task 2 (blocked)
behavior: smoke
depends_on:
  - $dep2_id
YAML
)"
printf '%s\n' "$child2_output"
child2_id="$(printf '%s\n' "$child2_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$child2_id" ]] || e2e_fail "failed to parse child2 id"

e2e_log "verifying start fails when dependency is not satisfied"
if "$E2E_BIN_DIR/boid" action send --task "$child2_id" --type start 2>&1; then
    e2e_fail "expected start to fail for task with unmet dependency, but it succeeded"
fi

child2_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$child2_id")"
printf '%s\n' "$child2_json"
e2e_assert_contains "$child2_json" '"status":"pending"'

# ============================================================
e2e_log "=== Test 3: 依存充足後に start 可能 ==="

e2e_run "$E2E_BIN_DIR/boid" action send --task "$dep2_id" --type start
e2e_run "$E2E_BIN_DIR/boid" action send --task "$dep2_id" --type done

# dep2 が done になると自動トリガーが child2 を自動 start するのを待つ
child2_exec_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 10s --interval 100ms "$child2_id" executing)"
printf '%s\n' "$child2_exec_json"
e2e_assert_contains "$child2_exec_json" '"status":"executing"'

# ============================================================
e2e_log "=== Test 4: auto_start + 未充足依存 → pending のまま ==="

dep4_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: auto-start-deps
title: Dep Task 4 (pending)
behavior: smoke
YAML
)"
printf '%s\n' "$dep4_output"
dep4_id="$(printf '%s\n' "$dep4_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$dep4_id" ]] || e2e_fail "failed to parse dep4 id"

child4_output="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: auto-start-deps
title: Child Task 4 (auto_start + unmet dep)
behavior: smoke
auto_start: true
depends_on:
  - $dep4_id
YAML
)"
printf '%s\n' "$child4_output"
child4_id="$(printf '%s\n' "$child4_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$child4_id" ]] || e2e_fail "failed to parse child4 id"

child4_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$child4_id")"
printf '%s\n' "$child4_json"
e2e_assert_contains "$child4_json" '"status":"pending"'

# ============================================================
e2e_log "=== Test 5: auto_start + 充足済み依存 → 自動 start ==="

e2e_run "$E2E_BIN_DIR/boid" action send --task "$dep4_id" --type start
e2e_run "$E2E_BIN_DIR/boid" action send --task "$dep4_id" --type done

child5_output="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: auto-start-deps
title: Child Task 5 (auto_start + met dep)
behavior: smoke
auto_start: true
depends_on:
  - $dep4_id
YAML
)"
printf '%s\n' "$child5_output"
child5_id="$(printf '%s\n' "$child5_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$child5_id" ]] || e2e_fail "failed to parse child5 id"

child5_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 10s --interval 100ms "$child5_id" executing)"
printf '%s\n' "$child5_json"
e2e_assert_contains "$child5_json" '"status":"executing"'

# ============================================================
e2e_log "=== Test 6: depends_on_payload ==="

# 正ケース: result: true
dep6_pos_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: auto-start-deps
title: Dep Task 6 Positive
behavior: smoke
payload:
  result: true
YAML
)"
printf '%s\n' "$dep6_pos_output"
dep6_pos_id="$(printf '%s\n' "$dep6_pos_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$dep6_pos_id" ]] || e2e_fail "failed to parse dep6_pos id"

e2e_run "$E2E_BIN_DIR/boid" action send --task "$dep6_pos_id" --type start
e2e_run "$E2E_BIN_DIR/boid" action send --task "$dep6_pos_id" --type done

child6_pos_output="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: auto-start-deps
title: Child Task 6 Positive
behavior: smoke
depends_on:
  - $dep6_pos_id
depends_on_payload: result
YAML
)"
printf '%s\n' "$child6_pos_output"
child6_pos_id="$(printf '%s\n' "$child6_pos_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$child6_pos_id" ]] || e2e_fail "failed to parse child6_pos id"

e2e_log "verifying start succeeds when depends_on_payload is truthy"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$child6_pos_id" --type start

# 負ケース: result: false
dep6_neg_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: auto-start-deps
title: Dep Task 6 Negative
behavior: smoke
payload:
  result: false
YAML
)"
printf '%s\n' "$dep6_neg_output"
dep6_neg_id="$(printf '%s\n' "$dep6_neg_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$dep6_neg_id" ]] || e2e_fail "failed to parse dep6_neg id"

e2e_run "$E2E_BIN_DIR/boid" action send --task "$dep6_neg_id" --type start
e2e_run "$E2E_BIN_DIR/boid" action send --task "$dep6_neg_id" --type done

child6_neg_output="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: auto-start-deps
title: Child Task 6 Negative
behavior: smoke
depends_on:
  - $dep6_neg_id
depends_on_payload: result
YAML
)"
printf '%s\n' "$child6_neg_output"
child6_neg_id="$(printf '%s\n' "$child6_neg_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$child6_neg_id" ]] || e2e_fail "failed to parse child6_neg id"

e2e_log "verifying start fails when depends_on_payload is falsy"
if "$E2E_BIN_DIR/boid" action send --task "$child6_neg_id" --type start 2>&1; then
    e2e_fail "expected start to fail for falsy depends_on_payload, but it succeeded"
fi
