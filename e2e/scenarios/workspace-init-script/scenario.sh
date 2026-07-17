#!/usr/bin/env bash
set -euo pipefail

# docs/plans/home-workspace-volume.md Phase 4 PR6, scenario C: pins that a
# workspace's init.sh (internal/dispatcher/workspace_home.go's
# resolveWorkspaceHome) runs exactly once per distinct script content —
# not once per dispatch — and reruns only when the script's content (hash)
# actually changes.
#
# init.sh lives at $XDG_CONFIG_HOME/boid/workspaces/<slug>/init.sh (this
# harness isolates XDG_CONFIG_HOME per scenario, see e2e/run.sh), which this
# scenario plants directly before ever dispatching into the workspace. The
# completion marker lives outside the workspace home
# ($XDG_DATA_HOME/boid/homes/<slug>.init.json — see workspaceHomeMarkerPath),
# and the workspace home itself resolves to
# $XDG_DATA_HOME/boid/homes/<slug>/ (WorkspaceHomesDir derives this from the
# daemon's --db-path, which run.sh points at $XDG_DATA_HOME/boid/boid.db).
# Both paths are on the host filesystem this scenario.sh itself runs on (the
# boid daemon is a plain host process in this harness, not containerized),
# so the init script's own side effect — appending a line to
# $BOID_WORKSPACE_HOME/.init-log — can be asserted directly with `wc -l`
# instead of needing to round-trip through a sandboxed job.

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
WS_SLUG="pr6-init-once"
INIT_SCRIPT_DIR="$XDG_CONFIG_HOME/boid/workspaces/$WS_SLUG"
INIT_LOG="$XDG_DATA_HOME/boid/homes/$WS_SLUG/.init-log"

e2e_log "planting initial init.sh for $WS_SLUG"
mkdir -p "$INIT_SCRIPT_DIR"
cat > "$INIT_SCRIPT_DIR/init.sh" <<'EOF'
#!/bin/bash
echo "run-$(date +%s%N)" >> "$BOID_WORKSPACE_HOME/.init-log"
EOF

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "creating workspace $WS_SLUG"
e2e_run "$E2E_BIN_DIR/boid" workspace create "$WS_SLUG"

e2e_log "assigning project to $WS_SLUG"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "e2e-init-once" "$WS_SLUG"

run_ping_task() {
  local title="$1"
  local out id json

  out="$("$E2E_BIN_DIR/boid" task create <<YAML
project_id: e2e-init-once
title: $title
behavior: ping
YAML
)"
  printf '%s\n' "$out"
  id="$(printf '%s\n' "$out" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
  [[ -n "$id" ]] || e2e_fail "failed to parse task id for $title"

  e2e_run "$E2E_BIN_DIR/boid" action send --task "$id" --type start

  json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 30s --interval 200ms "$id" done)"
  printf '%s\n' "$json"
  e2e_assert_contains "$json" '"status":"done"'
}

e2e_log "=== task 1: first dispatch into $WS_SLUG — init.sh must run ==="
run_ping_task "Ping 1"
e2e_wait_for_file "$INIT_LOG"
line_count="$(wc -l < "$INIT_LOG" | tr -d ' ')"
[[ "$line_count" == "1" ]] || e2e_fail "expected .init-log to have 1 line after first dispatch, got $line_count"

e2e_log "=== task 2: second dispatch, same init.sh content — must NOT rerun ==="
run_ping_task "Ping 2"
line_count="$(wc -l < "$INIT_LOG" | tr -d ' ')"
[[ "$line_count" == "1" ]] || e2e_fail "expected .init-log to still have 1 line after second dispatch (init.sh unchanged), got $line_count"

e2e_log "=== modifying init.sh content ==="
cat > "$INIT_SCRIPT_DIR/init.sh" <<'EOF'
#!/bin/bash
echo "modified-$(date +%s%N)" >> "$BOID_WORKSPACE_HOME/.init-log"
EOF

e2e_log "=== task 3: dispatch after init.sh content changed — must rerun ==="
run_ping_task "Ping 3"
line_count="$(wc -l < "$INIT_LOG" | tr -d ' ')"
[[ "$line_count" == "2" ]] || e2e_fail "expected .init-log to have 2 lines after init.sh content changed, got $line_count"
grep -q "modified-" "$INIT_LOG" || e2e_fail "expected .init-log's second run to be the modified script (missing 'modified-' line)"
