#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# shellcheck source=/dev/null
source "$SCRIPT_DIR/lib/common.sh"

KEEP_TEMP=0
REQUESTED_SCENARIO=""
RUN_ALL=1

usage() {
  cat <<'EOF'
usage: ./e2e/run.sh [--keep-temp] [scenario]
If no scenario is provided, all scenarios under e2e/scenarios/ are run.
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
if [[ $RUN_ALL -eq 1 ]]; then
  mapfile -t scenario_dirs < <(find "$SCRIPT_DIR/scenarios" -mindepth 1 -maxdepth 1 -type d | sort)
  for scenario_dir in "${scenario_dirs[@]}"; do
    scenarios+=("$(basename "$scenario_dir")")
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
  local scenario_dir="$SCRIPT_DIR/scenarios/$scenario"

  [[ -d "$scenario_dir" ]] || e2e_fail "scenario not found: $scenario"
  [[ -f "$scenario_dir/scenario.sh" ]] || e2e_fail "scenario script not found: $scenario_dir/scenario.sh"

  if [[ -f "$scenario_dir/requires-sandbox" ]]; then
    e2e_log "checking sandbox prerequisites"
    e2e_require_sandbox_prereqs
  fi

  (
    set -euo pipefail

    ROOT="$(mktemp -d "${TMPDIR:-/tmp}/boid-e2e-${scenario}-XXXXXX")"
    SERVER_PID=""

    cleanup() {
      local exit_code=$?

      if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" >/dev/null 2>&1; then
        kill -TERM "$SERVER_PID" >/dev/null 2>&1 || true
        wait "$SERVER_PID" >/dev/null 2>&1 || true
      fi

      if [[ -n "${BOID_E2E_TMUX_SESSION:-}" ]] && command -v tmux >/dev/null 2>&1; then
        TMUX_TMPDIR="${TMUX_TMPDIR:-}" tmux kill-session -t "$BOID_E2E_TMUX_SESSION" >/dev/null 2>&1 || true
      fi

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
    export TMUX_TMPDIR="$ROOT/tmux"
    export E2E_ROOT="$ROOT"
    export E2E_STATE_DIR="$ROOT/state"
    export E2E_BIN_DIR="$ROOT/bin"
    export E2E_LOG_DIR="$ROOT/logs"
    export E2E_WORKSPACE_DIR="$ROOT/workspace"
    export BOID_E2E_TMUX_SESSION="boid-e2e-${scenario}-$$"
    export PATH="$E2E_BIN_DIR:$PATH"

    mkdir -p "$HOME" "$XDG_DATA_HOME/boid/kits" "$XDG_RUNTIME_DIR" "$TMUX_TMPDIR" "$E2E_STATE_DIR" "$E2E_BIN_DIR" "$E2E_LOG_DIR" "$E2E_WORKSPACE_DIR"

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

    e2e_log "starting boid server"
    "$E2E_BIN_DIR/boid" start \
      --db-path "$XDG_DATA_HOME/boid/boid.db" \
      --socket-path "$BOID_SOCKET" \
      --http-addr "127.0.0.1:0" \
      --tmux-session "$BOID_E2E_TMUX_SESSION" \
      --kits-dir "$XDG_DATA_HOME/boid/kits" \
      >"$E2E_LOG_DIR/server.stdout.log" \
      2>"$E2E_LOG_DIR/server.stderr.log" &
    SERVER_PID=$!

    e2e_run "$E2E_BIN_DIR/boid-e2e" wait-health --timeout 15s --interval 100ms "$BOID_SOCKET"

    e2e_log "running scenario: $scenario"
    (
      # shellcheck source=/dev/null
      source "$scenario_dir/scenario.sh"
    ) > >(tee "$E2E_LOG_DIR/scenario.log") 2>&1

    e2e_log "scenario completed successfully"
  )
}

for scenario in "${scenarios[@]}"; do
  run_scenario "$scenario"
done
