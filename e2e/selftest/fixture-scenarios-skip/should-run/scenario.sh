#!/usr/bin/env bash
set -euo pipefail

# The control case for run_sh_skip_marker_test.sh: a scenario with no `skip`
# marker must still run normally in a full ("run everything", no scenario
# name given) invocation.
e2e_log "should-run: executed as expected"
