#!/usr/bin/env bash
set -euo pipefail

# Deliberately fails via e2e_fail (used by real scenarios' assertion
# helpers, e2e_assert_contains in particular). This is the fixture scenario
# e2e/selftest/run_sh_exit_status_test.sh uses to pin that run.sh actually
# propagates a failing scenario's exit status as a nonzero run.sh exit code
# instead of silently reporting "scenario completed successfully" — see
# run.sh's own comment on the run_scenario function for the two independent
# bugs this guards against.
e2e_log "fixture-scenarios/fails: about to fail on purpose"
e2e_fail "fixture-scenarios/fails: intentional failure for the run.sh regression test"
