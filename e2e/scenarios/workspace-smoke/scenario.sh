#!/usr/bin/env bash
set -euo pipefail

DIR_A="$E2E_WORKSPACE_DIR/app-a"
DIR_B="$E2E_WORKSPACE_DIR/app-b"

# Step 1: Register two projects
e2e_log "registering project-a from $DIR_A"
e2e_run "$E2E_BIN_DIR/boid" project add "$DIR_A"

e2e_log "registering project-b from $DIR_B"
e2e_run "$E2E_BIN_DIR/boid" project add "$DIR_B"

# Step 1.5: Create the ws-1 workspace explicitly (docs/plans/
# workspace-db-consolidation.md PR4 Step J reinstates the workspaces-row
# existence check on assign — MAJOR 5 — so a slug with neither a DB row nor
# a local workspace.yaml now 404s instead of the pre-PR4 "get-or-create"
# behavior this scenario used to rely on).
e2e_log "creating workspace ws-1"
e2e_run "$E2E_BIN_DIR/boid" workspace create ws-1

# Step 2: Assign project-a to ws-1
e2e_log "assigning project-a to ws-1"
assign_out="$("$E2E_BIN_DIR/boid" workspace assign project-a ws-1)"
printf '%s\n' "$assign_out"
e2e_assert_contains "$assign_out" "project-a"
e2e_assert_contains "$assign_out" "ws-1"

# Step 3: Verify project-a is in ws-1 via workspace list
e2e_log "verifying ws-1 appears in workspace list"
ws_list="$("$E2E_BIN_DIR/boid" workspace list)"
printf '%s\n' "$ws_list"
e2e_assert_contains "$ws_list" "ws-1"

# Step 4: Assign project-b to ws-1
e2e_log "assigning project-b to ws-1"
assign_out="$("$E2E_BIN_DIR/boid" workspace assign project-b ws-1)"
printf '%s\n' "$assign_out"
e2e_assert_contains "$assign_out" "project-b"
e2e_assert_contains "$assign_out" "ws-1"

# Step 5: Verify both projects are reflected in ws-1 (count = 2)
e2e_log "verifying ws-1 has 2 projects"
ws_list="$("$E2E_BIN_DIR/boid" workspace list)"
printf '%s\n' "$ws_list"
e2e_assert_contains "$ws_list" "ws-1"
# PR4 list format (docs/plans/workspace-db-consolidation.md Step H):
# tabular columns SLUG / PROJECTS / REVISION — the yaml/DB "state"
# classification (ready/unconfigured/empty) is gone now that the
# workspaces table is the single source of truth (Step B).
# Grab the projects count (2nd column) from the ws-1 row.
ws1_count="$(echo "$ws_list" | grep "ws-1" | awk '{print $2}')"
if [ "$ws1_count" != "2" ]; then
  echo "ERROR: expected ws-1 project count 2, got '$ws1_count'"
  exit 1
fi

# Step 6: Clear project-a's workspace assignment
e2e_log "clearing project-a workspace assignment"
clear_out="$("$E2E_BIN_DIR/boid" workspace clear project-a)"
printf '%s\n' "$clear_out"
e2e_assert_contains "$clear_out" "project-a"

# Step 7: Verify project-a is gone from ws-1 (count drops to 1)
e2e_log "verifying ws-1 has 1 project after clearing project-a"
ws_list="$("$E2E_BIN_DIR/boid" workspace list)"
printf '%s\n' "$ws_list"
e2e_assert_contains "$ws_list" "ws-1"
# Grab the projects count (2nd column) from the ws-1 row.
ws1_count="$(echo "$ws_list" | grep "ws-1" | awk '{print $2}')"
if [ "$ws1_count" != "1" ]; then
  echo "ERROR: expected ws-1 project count 1, got '$ws1_count'"
  exit 1
fi
