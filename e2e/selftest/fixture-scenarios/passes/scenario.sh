#!/usr/bin/env bash
set -euo pipefail

# Deliberately succeeds — the control case for
# e2e/selftest/run_sh_exit_status_test.sh, pinning that a normal successful
# scenario still reports run.sh exit code 0 (guards against an
# overcorrection that makes run.sh report failure unconditionally).
e2e_log "fixture-scenarios/passes: nothing to do, succeeding"
