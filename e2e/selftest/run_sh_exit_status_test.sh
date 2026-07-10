#!/usr/bin/env bash
set -uo pipefail
# Deliberately NOT `set -e`: this script's whole point is to check the exit
# codes of sub-invocations of run.sh itself, so every invocation below is
# followed by an explicit status check rather than relying on errexit.

# Regression guard for a real, previously-shipped bug found alongside the
# git gateway cutover exec-dispatch work (PR #735 / #736): e2e/run.sh used
# to report every scenario as successful regardless of whether the scenario
# itself failed, because of two independent bugs (see run.sh's own comment
# on run_scenario for the full story):
#
#   1. `(source scenario.sh) > >(tee log) 2>&1` discards the compound
#      command's exit status entirely (a process-substitution redirection
#      quirk).
#   2. run_scenario is called as `if run_scenario "$scenario"; then ...`,
#      and POSIX/bash suspend the errexit *action* — not the -e option's
#      value — for the whole evaluation of an if/while/until condition,
#      including subshells that re-assert `set -e` themselves.
#
# Both bugs together meant a scenario could print
# "[e2e] ERROR: ..." and still have run.sh exit 0, which is exactly the
# false-positive CI green that went unnoticed since PR6 (see PR #736 for the
# reproduction against real CI logs). This script pins both directions:
# a failing scenario must make run.sh exit nonzero, and a passing scenario
# must still make run.sh exit zero (guarding against an overcorrection that
# reports every scenario as a failure).
#
# The fixture scenarios live in e2e/selftest/fixture-scenarios/, entirely
# outside e2e/scenarios/, so they never show up in a normal `./e2e/run.sh`
# (no-argument, run-everything) invocation — only this script's explicit
# E2E_SCENARIOS_ROOT override reaches them.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
FIXTURE_SCENARIOS="$SCRIPT_DIR/fixture-scenarios"

fail_count=0

run_case() {
  local name="$1"
  local scenario="$2"
  local want_exit="$3"

  echo "[run_sh_exit_status_test] running case: $name (scenario=$scenario, want_exit=$want_exit)"
  E2E_SCENARIOS_ROOT="$FIXTURE_SCENARIOS" E2E_MAX_ATTEMPTS=1 \
    "$REPO_ROOT/e2e/run.sh" "$scenario" >/tmp/run_sh_exit_status_test.$scenario.log 2>&1
  local got_exit=$?

  if [[ "$got_exit" -eq "$want_exit" ]]; then
    echo "[run_sh_exit_status_test] PASS: $name (run.sh exit code = $got_exit, want $want_exit)"
  else
    echo "[run_sh_exit_status_test] FAIL: $name (run.sh exit code = $got_exit, want $want_exit)"
    echo "----- captured output -----"
    cat "/tmp/run_sh_exit_status_test.$scenario.log"
    echo "----------------------------"
    fail_count=$((fail_count + 1))
  fi
}

# A deliberately failing scenario must make run.sh exit nonzero.
run_case "failing scenario propagates as a nonzero run.sh exit" "fails" 1

# A deliberately succeeding scenario must still make run.sh exit zero
# (the control case — without it, a fix that makes run.sh always report
# failure would still pass the case above).
run_case "passing scenario still exits zero" "passes" 0

if [[ "$fail_count" -gt 0 ]]; then
  echo "[run_sh_exit_status_test] $fail_count case(s) failed"
  exit 1
fi

echo "[run_sh_exit_status_test] all cases passed"
