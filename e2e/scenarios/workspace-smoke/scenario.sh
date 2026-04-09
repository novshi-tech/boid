#!/usr/bin/env bash
set -euo pipefail

DIR_A="$E2E_WORKSPACE_DIR/app-a"
DIR_B="$E2E_WORKSPACE_DIR/app-b"

# Step 1: Register two projects
e2e_log "registering project-a from $DIR_A"
e2e_run "$E2E_BIN_DIR/boid" project add "$DIR_A"

e2e_log "registering project-b from $DIR_B"
e2e_run "$E2E_BIN_DIR/boid" project add "$DIR_B"

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
e2e_assert_contains "$ws_list" "2 projects"

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
e2e_assert_contains "$ws_list" "1 projects"
