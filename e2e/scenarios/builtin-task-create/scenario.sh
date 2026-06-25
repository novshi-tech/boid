#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

# Set up workspace for new schema (PR4 hard cutover).
# Two workspaces are needed because parent and hook-parent behaviors each
# have a `spawn` hook that writes the `artifact` trait. Loading both kits
# at the same workspace makes every behavior fire both hooks, producing
# an exclusive-trait collision. We assign one workspace for the parent
# scenario and switch to the other before the hook-parent scenario.
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/builtin-task-create.yaml" <<YAML
kits:
  - github.com/novshi-tech/boid-kits/builtin-task-create
YAML
cat > "$XDG_CONFIG_HOME/boid/workspaces/hook-task-create.yaml" <<YAML
kits:
  - github.com/novshi-tech/boid-kits/hook-task-create
YAML

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "e2e-builtin-task-create" "builtin-task-create"

# ============================================================
e2e_log "=== creating parent task that triggers spawn hook ==="

parent_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: e2e-builtin-task-create
title: Parent Task
behavior: parent
YAML
)"
printf '%s\n' "$parent_output"
parent_id="$(printf '%s\n' "$parent_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$parent_id" ]] || e2e_fail "failed to parse parent id"

e2e_log "starting parent task $parent_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$parent_id" --type start

e2e_log "waiting for parent to reach done (hook fires during executing)"
parent_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 60s --interval 200ms "$parent_id" done)"
printf '%s\n' "$parent_json"
e2e_assert_contains "$parent_json" '"status":"done"'

# ============================================================
e2e_log "=== resolving subtask ids by title ==="

resolve_id() {
    local title="$1"
    "$E2E_BIN_DIR/boid" task list | awk -v t="$title" '$0 ~ t {print $1; exit}'
}

task_a_id="$(resolve_id 'Task A')"
task_b_id="$(resolve_id 'Task B')"
task_c_id="$(resolve_id 'Task C')"
task_d_id="$(resolve_id 'Task D')"

[[ -n "$task_a_id" ]] || e2e_fail "Task A not found — hook did not create subtask-a"
[[ -n "$task_b_id" ]] || e2e_fail "Task B not found — hook did not create subtask-b"
[[ -n "$task_c_id" ]] || e2e_fail "Task C not found — hook did not create subtask-c"
[[ -n "$task_d_id" ]] || e2e_fail "Task D not found — hook did not create subtask-d"

e2e_log "subtask ids: a=$task_a_id b=$task_b_id c=$task_c_id d=$task_d_id"

# ============================================================
e2e_log "=== verifying task-a fields ==="
task_a_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$task_a_id")"
printf '%s\n' "$task_a_json"
e2e_assert_contains "$task_a_json" '"ref":"task-a"'
e2e_assert_contains "$task_a_json" "\"parent_id\":\"$parent_id\""

# ============================================================
e2e_log "=== verifying task-b fields ==="
task_b_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$task_b_id")"
printf '%s\n' "$task_b_json"
e2e_assert_contains "$task_b_json" '"ref":"task-b"'
e2e_assert_contains "$task_b_json" "\"parent_id\":\"$parent_id\""

# ============================================================
e2e_log "=== verifying task-c fields (depends on a + b, auto_start) ==="
task_c_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$task_c_id")"
printf '%s\n' "$task_c_json"
e2e_assert_contains "$task_c_json" '"ref":"task-c"'
e2e_assert_contains "$task_c_json" "\"parent_id\":\"$parent_id\""
e2e_assert_contains "$task_c_json" '"auto_start":true'

# ============================================================
e2e_log "=== verifying task-d fields (depends on a only, auto_start) ==="
task_d_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$task_d_id")"
printf '%s\n' "$task_d_json"
e2e_assert_contains "$task_d_json" '"ref":"task-d"'
e2e_assert_contains "$task_d_json" "\"parent_id\":\"$parent_id\""
e2e_assert_contains "$task_d_json" '"auto_start":true'

# ============================================================
e2e_log "=== hook task create: hook で boid task_create が呼べることを確認 ==="

# Switch to the hook-task-create workspace so the hook-parent behavior fires
# only the hook-task-create kit's spawn (not both kits, which collide on the
# artifact trait).
e2e_run "$E2E_BIN_DIR/boid" workspace assign "e2e-builtin-task-create" "hook-task-create"

hook_parent_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: e2e-builtin-task-create
title: Hook Parent Task
behavior: hook-parent
YAML
)"
printf '%s\n' "$hook_parent_output"
hook_parent_id="$(printf '%s\n' "$hook_parent_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$hook_parent_id" ]] || e2e_fail "failed to parse hook parent task id"

e2e_log "starting hook-parent task $hook_parent_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$hook_parent_id" --type start

e2e_log "waiting for hook-parent to reach done"
hook_parent_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 30s --interval 200ms "$hook_parent_id" done)"
printf '%s\n' "$hook_parent_json"
e2e_assert_contains "$hook_parent_json" '"status":"done"'

e2e_log "verifying hook-spawned subtask exists"
spawned_id="$("$E2E_BIN_DIR/boid" task list | awk '/Hook Spawned Task/{print $1; exit}')"
[[ -n "$spawned_id" ]] || e2e_fail "Hook Spawned Task not found — hook did not call boid task create"
spawned_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$spawned_id")"
e2e_assert_contains "$spawned_json" '"ref":"hook-spawned"'
