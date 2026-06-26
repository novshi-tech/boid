#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

# Set up workspace for new schema (PR4 hard cutover).
# The kit is a tool-supply placeholder (no env/host_commands/bindings) and
# does not provide hooks under the current schema. The spawn hook lives in
# project.yaml's task_behaviors and references a script under .boid/hooks/.
WS_SLUG="idempotent-task-create"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - idempotent-task-create
YAML

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "e2e-idempotent-task-create" "$WS_SLUG"

# ============================================================
e2e_log "=== creating parent task that triggers idempotent spawn hook ==="

parent_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: e2e-idempotent-task-create
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
e2e_log "=== verifying get-or-create: both task IDs must be equal ==="

first_id="$("$E2E_BIN_DIR/boid" task artifacts "$parent_id" --field first_id)"
second_id="$("$E2E_BIN_DIR/boid" task artifacts "$parent_id" --field second_id)"

e2e_log "first_id=$first_id  second_id=$second_id"

[[ -n "$first_id" ]]  || e2e_fail "artifact.first_id is empty — hook did not record first create"
[[ -n "$second_id" ]] || e2e_fail "artifact.second_id is empty — hook did not record second create"
[[ "$first_id" == "$second_id" ]] || \
    e2e_fail "get-or-create failed: first=$first_id != second=$second_id (duplicate child was created)"

# ============================================================
e2e_log "=== verifying exactly one child task with ref step-a exists ==="

child_count="$("$E2E_BIN_DIR/boid" task list | awk -v t="Step A" '$0 ~ t {count++} END{print count+0}')"
e2e_log "tasks with title 'Step A': $child_count"
[[ "$child_count" -eq 1 ]] || \
    e2e_fail "expected exactly 1 child task with title 'Step A', got $child_count (duplicate created)"

e2e_log "=== idempotent-task-create: all assertions passed ==="
