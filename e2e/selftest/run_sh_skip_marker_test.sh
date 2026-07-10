#!/usr/bin/env bash
set -uo pipefail
# Deliberately NOT `set -e`: see run_sh_exit_status_test.sh's own header for
# why (this script checks a sub-invocation's exit code explicitly instead).

# Regression guard for the `skip` marker mechanism added alongside PR #736's
# e2e triage: worktree-lifecycle / exec-smoke / git-peer-clone-local carry
# real, tracked reasons (pre-cutover scenario content, or a PR-#735 merge
# dependency — see each scenario's own `skip` file) for not running in the
# default "run everything" pass. docs/plans/quality-gates.md's "no silent
# caps" principle means that opt-out must never be silent: this test proves
# (a) a `skip`-marked scenario is excluded from a full run.sh invocation
# entirely (its body never executes — we'd know, because it's deliberately
# written to fail hard if it ever does) and (b) the skip is still logged and
# counted, not just dropped.
#
# The fixture scenarios live in e2e/selftest/fixture-scenarios-skip/,
# entirely outside e2e/scenarios/, so this never touches (or is affected by)
# the real scenario set CI runs by default — only this script's explicit
# E2E_SCENARIOS_ROOT override reaches them.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
FIXTURE_SCENARIOS="$SCRIPT_DIR/fixture-scenarios-skip"
LOG_FILE="/tmp/run_sh_skip_marker_test.log"

fail_count=0

check() {
  local description="$1"
  local ok="$2"
  if [[ "$ok" -eq 0 ]]; then
    echo "[run_sh_skip_marker_test] PASS: $description"
  else
    echo "[run_sh_skip_marker_test] FAIL: $description"
    fail_count=$((fail_count + 1))
  fi
}

echo "[run_sh_skip_marker_test] running full suite against $FIXTURE_SCENARIOS (no scenario name = run everything except skip-marked)"
E2E_SCENARIOS_ROOT="$FIXTURE_SCENARIOS" E2E_MAX_ATTEMPTS=1 \
  "$REPO_ROOT/e2e/run.sh" >"$LOG_FILE" 2>&1
got_exit=$?

[[ "$got_exit" -eq 0 ]]
check "run.sh exits 0 (the only non-skip scenario passes, the skip-marked one is never attempted)" $?

grep -q 'should-run: executed as expected' "$LOG_FILE"
check "the non-skip-marked scenario actually ran" $?

grep -q '\[e2e\]\[skip\].*should-skip.*skipped:' "$LOG_FILE"
check "the skip is logged individually at collection time, with its reason" $?

if grep -q 'this scenario ran despite having a skip marker' "$LOG_FILE"; then
  check "the skip-marked scenario's body never executed" 1
else
  check "the skip-marked scenario's body never executed" 0
fi

grep -q '\[e2e\]\[skip\] summary: skipped=1.*should-skip' "$LOG_FILE"
check "the aggregate skip summary is printed" $?

if [[ "$fail_count" -gt 0 ]]; then
  echo "[run_sh_skip_marker_test] $fail_count case(s) failed"
  echo "----- captured output -----"
  cat "$LOG_FILE"
  echo "----------------------------"
  exit 1
fi

echo "[run_sh_skip_marker_test] all cases passed"
