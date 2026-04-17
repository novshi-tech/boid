#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "testing echo command inside sandbox"
exec_out="$("$E2E_BIN_DIR/boid" exec -p e2e-exec echo-hello)"
printf '%s\n' "$exec_out"
e2e_assert_contains "$exec_out" "hello from sandbox"

e2e_log "testing host filesystem isolation"
sentinel="$E2E_STATE_DIR/host-sentinel.txt"
echo "host-only-secret" > "$sentinel"
if "$E2E_BIN_DIR/boid" exec -p e2e-exec cat-sentinel 2>/dev/null; then
    e2e_fail "expected host-only file to be inaccessible from sandbox"
fi
e2e_log "OK: host filesystem not accessible from sandbox"

e2e_log "testing boid builtin is available inside sandbox"
boid_path="$("$E2E_BIN_DIR/boid" exec -p e2e-exec which-boid)"
printf '%s\n' "$boid_path"
e2e_assert_contains "$boid_path" "boid"
