#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "testing echo argv inside sandbox"
exec_out="$("$E2E_BIN_DIR/boid" exec -p e2e-exec -- echo "hello from sandbox")"
printf '%s\n' "$exec_out"
e2e_assert_contains "$exec_out" "hello from sandbox"

e2e_log "testing host filesystem isolation"
sentinel="$E2E_STATE_DIR/host-sentinel.txt"
echo "host-only-secret" > "$sentinel"
if "$E2E_BIN_DIR/boid" exec -p e2e-exec -- cat "$sentinel" 2>/dev/null; then
    e2e_fail "expected host-only file to be inaccessible from sandbox"
fi
e2e_log "OK: host filesystem not accessible from sandbox"

e2e_log "testing boid builtin is available inside sandbox"
boid_path="$("$E2E_BIN_DIR/boid" exec -p e2e-exec -- which boid)"
printf '%s\n' "$boid_path"
e2e_assert_contains "$boid_path" "boid"

# git gateway cutover: `boid exec` now dispatches through Runner.Dispatch()
# (the same path every session job uses) instead of a client-side
# syscall.Exec, specifically so it picks up the sandbox-internal clone +
# gateway wiring the plain "echo"/"cat"/"which" checks above already
# exercise implicitly (they would fail with "spec.Clone is enabled but
# URL/TargetDir/Branch/BaseBranch must all be set" if that wiring regressed
# — see docs/plans/git-gateway-cutover.md and the dogfood bug this fixed).
# The two checks below pin the specific new behaviors that migration had to
# preserve: exact exit code propagation (non-interactive transport) and
# piped stdin forwarding (RuntimeStartSpec.StdinForward).
e2e_log "testing exact exit code propagation (non-interactive transport)"
set +e
"$E2E_BIN_DIR/boid" exec -p e2e-exec -- bash -c 'exit 42'
exec_status=$?
set -e
if [[ "$exec_status" -ne 42 ]]; then
    e2e_fail "expected boid exec to propagate exit code 42, got $exec_status"
fi
e2e_log "OK: exit code 42 propagated"

e2e_log "testing piped stdin is forwarded to the sandboxed command"
piped_out="$(printf 'piped payload\n' | "$E2E_BIN_DIR/boid" exec -p e2e-exec -- cat)"
printf '%s\n' "$piped_out"
e2e_assert_contains "$piped_out" "piped payload"
