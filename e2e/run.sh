#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# shellcheck source=/dev/null
source "$SCRIPT_DIR/lib/common.sh"

# SCENARIOS_ROOT defaults to the real e2e/scenarios/ directory but can be
# overridden via E2E_SCENARIOS_ROOT — used by
# e2e/selftest/run_sh_exit_status_test.sh (the regression guard for the
# run.sh exit-status-propagation bug) to point run.sh at a throwaway
# fixture scenario without ever touching the real scenario set CI runs by
# default.
SCENARIOS_ROOT="${E2E_SCENARIOS_ROOT:-$SCRIPT_DIR/scenarios}"

KEEP_TEMP=0
REQUESTED_SCENARIO=""
RUN_ALL=1

usage() {
  cat <<'EOF'
usage: ./e2e/run.sh [--keep-temp] [scenario]
If no scenario is provided, all scenarios under e2e/scenarios/ are run,
except those carrying a `skip` marker file (its contents are the tracked
reason — see e.g. e2e/scenarios/worktree-lifecycle/skip). Requesting a
scenario by name always runs it regardless of that marker.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --keep-temp)
      KEEP_TEMP=1
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      REQUESTED_SCENARIO="$1"
      RUN_ALL=0
      shift
      ;;
  esac
done

scenarios=()
skipped_names=()
skipped_reasons=()
if [[ $RUN_ALL -eq 1 ]]; then
  mapfile -t scenario_dirs < <(find "$SCENARIOS_ROOT" -mindepth 1 -maxdepth 1 -type d | sort)
  for scenario_dir in "${scenario_dirs[@]}"; do
    scenario_name="$(basename "$scenario_dir")"
    # `skip` (distinct from skip-e2e-upstream, which only skips the fixture
    # upstream helper for a scenario that still runs) opts a scenario out of
    # the default "run everything" pass entirely. Every skip must carry a
    # reason and a tracking destination (a future PR, an issue, ...) in the
    # marker file's own contents — silently dropping a known-broken scenario
    # from the suite with no visible trace is exactly the kind of cap
    # docs/plans/quality-gates.md's "no silent caps" principle rules out.
    # Requesting a scenario by name (RUN_ALL=0) bypasses this: an explicit
    # request is assumed to be a developer deliberately debugging it.
    if [[ -f "$scenario_dir/skip" ]]; then
      reason="$(<"$scenario_dir/skip")"
      skipped_names+=("$scenario_name")
      skipped_reasons+=("$reason")
      printf '[e2e][skip] scenario %q skipped: %s\n' "$scenario_name" "$reason" >&2
      continue
    fi
    scenarios+=("$scenario_name")
  done
else
  scenarios=("$REQUESTED_SCENARIO")
fi

if [[ ${#scenarios[@]} -eq 0 ]]; then
  e2e_fail "no scenarios found"
fi

e2e_require_cmd go

BUILD_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/boid-e2e-build-XXXXXX")"

cleanup_build() {
  rm -rf "$BUILD_ROOT" >/dev/null 2>&1 || true
}
trap cleanup_build EXIT

e2e_log "building boid binary"
e2e_run go build -o "$BUILD_ROOT/boid" "$REPO_ROOT"
e2e_log "building boid-e2e helper"
e2e_run go build -o "$BUILD_ROOT/boid-e2e" "$REPO_ROOT/e2e/cmd/boid-e2e"

run_scenario() {
  local scenario="$1"
  local scenario_dir="$SCENARIOS_ROOT/$scenario"

  [[ -d "$scenario_dir" ]] || e2e_fail "scenario not found: $scenario"
  [[ -f "$scenario_dir/scenario.sh" ]] || e2e_fail "scenario script not found: $scenario_dir/scenario.sh"

  if [[ -f "$scenario_dir/requires-sandbox" ]]; then
    e2e_log "checking sandbox prerequisites"
    e2e_require_sandbox_prereqs
  fi

  (
    set -euo pipefail

    ROOT="$(mktemp -d "${TMPDIR:-/tmp}/boid-e2e-${scenario}-XXXXXX")"

    cleanup() {
      local exit_code=$?

      # On failure, surface the daemon log and any retained runner-state.json so
      # CI (which discards the temp root) still shows why a sandbox launch failed.
      # daemon の slog 出力は server.stderr.log ではなく $HOME/.local/state/boid/boid.log
      # に行く (daemon.LogFilePath() を参照)。 失敗診断の主資料はこちら。
      if [[ $exit_code -ne 0 ]]; then
        if [[ -f "$E2E_LOG_DIR/server.stderr.log" ]]; then
          printf '[e2e] ===== server.stderr.log (tail) =====\n' >&2
          tail -n 200 "$E2E_LOG_DIR/server.stderr.log" >&2 || true
        fi
        if [[ -f "$E2E_LOG_DIR/upstream.stderr.log" ]]; then
          printf '[e2e] ===== upstream.stderr.log (tail) =====\n' >&2
          tail -n 200 "$E2E_LOG_DIR/upstream.stderr.log" >&2 || true
        fi
        local daemon_log="$HOME/.local/state/boid/boid.log"
        if [[ -f "$daemon_log" ]]; then
          printf '[e2e] ===== %s (tail) =====\n' "$daemon_log" >&2
          tail -n 200 "$daemon_log" >&2 || true
        fi
        for sf in /tmp/boid-*-runner-state.json; do
          [[ -f "$sf" ]] || continue
          printf '[e2e] ===== %s =====\n' "$sf" >&2
          cat "$sf" >&2 || true
        done
      fi

      # docs/plans/git-gateway-cutover.md PR6 cutover (PR7a Opus heads-up):
      # kill the fixture upstream server BEFORE `boid stop`, not after. Post-
      # cutover, an in-flight sandbox job may still be mid `git clone`/fetch
      # against this upstream when the scenario ends; killing it first makes
      # that connection fail fast (refused/reset) so the job winds down
      # immediately instead of hanging on a live-but-abandoned socket while
      # `boid stop` waits for it — the previous order (stop, then kill
      # upstream) left the upstream server alive exactly while the daemon was
      # trying to shut down around such a job.
      if [[ -n "${E2E_UPSTREAM_PID:-}" ]]; then
        kill "$E2E_UPSTREAM_PID" >/dev/null 2>&1 || true
        wait "$E2E_UPSTREAM_PID" 2>/dev/null || true
      fi

      "$E2E_BIN_DIR/boid" stop >/dev/null 2>&1 || true

      if [[ $exit_code -ne 0 || $KEEP_TEMP -eq 1 ]]; then
        printf '[e2e] temp root preserved at %s\n' "$ROOT" >&2
      else
        if ! rm -rf "$ROOT" >/dev/null 2>&1; then
          printf '[e2e] temp root preserved at %s (cleanup failed)\n' "$ROOT" >&2
        fi
      fi
    }
    trap cleanup EXIT

    export HOME="$ROOT/home"
    export XDG_DATA_HOME="$ROOT/data"
    export XDG_RUNTIME_DIR="$ROOT/run"
    export BOID_SOCKET="$ROOT/run/boid.sock"
    unset TMUX TMUX_PANE
    export E2E_ROOT="$ROOT"
    export E2E_STATE_DIR="$ROOT/state"
    export E2E_BIN_DIR="$ROOT/bin"
    export E2E_LOG_DIR="$ROOT/logs"
    export E2E_WORKSPACE_DIR="$ROOT/workspace"
    export PATH="$E2E_BIN_DIR:$PATH"

    export XDG_CONFIG_HOME="$ROOT/config"
    mkdir -p "$HOME" "$XDG_DATA_HOME/boid/kits" "$XDG_RUNTIME_DIR" "$E2E_STATE_DIR" "$E2E_BIN_DIR" "$E2E_LOG_DIR" "$E2E_WORKSPACE_DIR" "$XDG_CONFIG_HOME/boid"
    printf 'web:\n  http_addr: "127.0.0.1:0"\n' > "$XDG_CONFIG_HOME/boid/config.yaml"

    cp -f "$BUILD_ROOT/boid" "$E2E_BIN_DIR/boid"
    cp -f "$BUILD_ROOT/boid-e2e" "$E2E_BIN_DIR/boid-e2e"

    if [[ -d "$SCRIPT_DIR/fixtures/hostbin" ]]; then
      e2e_log "copying fake host commands"
      cp -R "$SCRIPT_DIR/fixtures/hostbin/." "$E2E_BIN_DIR/"
      find "$E2E_BIN_DIR" -maxdepth 1 -type f \( -name git -o -name gh -o -name systemctl \) -exec chmod +x {} +
    fi

    if [[ -d "$SCRIPT_DIR/fixtures/kits" ]]; then
      e2e_log "copying fixture kits"
      cp -R "$SCRIPT_DIR/fixtures/kits/." "$XDG_DATA_HOME/boid/kits/"
    fi

    if [[ -d "$scenario_dir/workspace" ]]; then
      e2e_log "copying scenario workspace"
      cp -R "$scenario_dir/workspace/." "$E2E_WORKSPACE_DIR/"
    fi

    if [[ -f "$scenario_dir/skip-e2e-upstream" ]]; then
      e2e_log "skipping fixture upstream setup (skip-e2e-upstream marker present)"
    else
      e2e_setup_fixture_upstream "$E2E_WORKSPACE_DIR"
    fi

    e2e_log "starting boid server"
    "$E2E_BIN_DIR/boid" start \
      --db-path "$XDG_DATA_HOME/boid/boid.db" \
      --socket-path "$BOID_SOCKET" \
      --kits-dir "$XDG_DATA_HOME/boid/kits" \
      --key-file-path "$XDG_DATA_HOME/boid/boid-secret.key" \
      >"$E2E_LOG_DIR/server.stdout.log" \
      2>"$E2E_LOG_DIR/server.stderr.log"

    e2e_run "$E2E_BIN_DIR/boid-e2e" wait-health --timeout 15s --interval 100ms "$BOID_SOCKET"

    e2e_log "running scenario: $scenario"
    # Two independent bugs used to hide behind this one line
    # (`(source scenario.sh) > >(tee log) 2>&1`):
    #
    # 1. Redirecting a compound command's own stdout/stderr to a process
    #    substitution discards its exit status entirely — bash reports the
    #    whole statement as successful even when the sourced scenario.sh
    #    calls `exit 1` (e.g. via e2e_assert_contains / e2e_fail), because
    #    the redirection target is an asynchronous process substitution
    #    rather than a plain file. Fixed by moving the redirection *inside*
    #    the subshell via `exec` instead of wrapping the subshell invocation
    #    in one: the compound command the caller evaluates is now just
    #    `(...)` with no redirection of its own, so `$?` right after it is
    #    exactly the sourced script's exit status.
    #
    # 2. That alone is still not sufficient: run_scenario is called as
    #    `if run_scenario "$scenario"; then ...`, and POSIX/bash suspend the
    #    errexit *action* (not the -e option's value) for every command run
    #    while evaluating an if/while/until condition — including commands
    #    inside subshells that re-assert `set -e` themselves, since the
    #    suspension is a separate internal "are we evaluating a condition"
    #    flag, not the errexit option. So even with fix #1, a failing
    #    scenario would fall through to "scenario completed successfully"
    #    and this subshell would exit with whatever its own *last* command
    #    naturally returns (0), silently discarding the real failure.
    #    Capturing the exit code explicitly and calling `exit` with it
    #    sidesteps errexit's suppressed-in-a-condition semantics entirely:
    #    `exit N` always terminates immediately regardless of -e's state.
    (
      exec > >(tee "$E2E_LOG_DIR/scenario.log") 2>&1
      # shellcheck source=/dev/null
      source "$scenario_dir/scenario.sh"
    )
    scenario_exit_code=$?
    if [[ $scenario_exit_code -ne 0 ]]; then
      exit "$scenario_exit_code"
    fi

    e2e_log "scenario completed successfully"
  )
}

# Automatic retry for the e2e gate's own reliability (docs/plans/quality-gates.md
# 前提 P). A single scenario flake must not paint a 60-minute job red — but a
# retry is a VISIBILITY tool, never a mask. Every retry is logged loudly with the
# grep-able "[e2e][retry]" marker, a retry that then passes is reported as a FLAKE
# to investigate (not swallowed), and a scenario that fails all attempts still
# fails the run. Default: 2 attempts (1 retry). Set E2E_MAX_ATTEMPTS=1 to disable.
E2E_MAX_ATTEMPTS="${E2E_MAX_ATTEMPTS:-2}"

retried_scenarios=()
failed_scenarios=()

for scenario in "${scenarios[@]}"; do
  attempt=1
  while true; do
    if run_scenario "$scenario"; then
      if [[ $attempt -gt 1 ]]; then
        # Passed only after a retry: this IS a flake. Surface it — do not ignore.
        printf '[e2e][retry] FLAKE: scenario %q passed on attempt %d/%d. This is a real flake; investigate the root cause, do not rely on the retry.\n' \
          "$scenario" "$attempt" "$E2E_MAX_ATTEMPTS" >&2
        retried_scenarios+=("$scenario")
      fi
      break
    fi
    if [[ $attempt -ge $E2E_MAX_ATTEMPTS ]]; then
      printf '[e2e][retry] scenario %q FAILED after %d attempt(s) — real failure.\n' \
        "$scenario" "$attempt" >&2
      failed_scenarios+=("$scenario")
      break
    fi
    printf '[e2e][retry] scenario %q failed on attempt %d; retrying (attempt %d/%d). A retry that then passes is reported as a FLAKE below, not swallowed.\n' \
      "$scenario" "$attempt" $((attempt + 1)) "$E2E_MAX_ATTEMPTS" >&2
    attempt=$((attempt + 1))
  done
done

# Machine-grep-able summary so CI-log aggregation can measure retry frequency
# without a dedicated basis (per plan: "CI ログ集計で足りる").
printf '[e2e][retry] summary: retried=%d (%s) failed=%d (%s)\n' \
  "${#retried_scenarios[@]}" "${retried_scenarios[*]:-none}" \
  "${#failed_scenarios[@]}" "${failed_scenarios[*]:-none}" >&2

# Always printed (success or failure) — an explicitly skipped scenario is
# never silent, per docs/plans/quality-gates.md's "no silent caps" principle.
# Each skipped scenario's own reason was already logged individually above
# at collection time; this is just the aggregate count/name list for
# CI-log aggregation, mirroring the retry summary's shape.
printf '[e2e][skip] summary: skipped=%d (%s)\n' \
  "${#skipped_names[@]}" "${skipped_names[*]:-none}" >&2

if [[ ${#failed_scenarios[@]} -gt 0 ]]; then
  e2e_fail "e2e scenarios failed after retries: ${failed_scenarios[*]}"
fi
