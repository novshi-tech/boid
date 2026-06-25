#!/usr/bin/env bash
set -euo pipefail

# Start fake Docker upstream socket at $XDG_RUNTIME_DIR/docker.sock.
FAKE_DOCKER_LOG="$E2E_STATE_DIR/fake-docker.log"
"$E2E_BIN_DIR/boid-e2e" fake-docker --log "$FAKE_DOCKER_LOG" "$XDG_RUNTIME_DIR/docker.sock" &
FAKE_DOCKER_PID=$!
trap 'kill "$FAKE_DOCKER_PID" 2>/dev/null || true' EXIT

e2e_run "$E2E_BIN_DIR/boid-e2e" wait-unix-socket --timeout 5s "$XDG_RUNTIME_DIR/docker.sock"

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
# Set up workspace for new schema (PR4 hard cutover).
WS_SLUG="docker-proxy-reap-on-failure"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - docker-proxy-test
env:
  DOCKER_PROXY_TEST_CASE: reap-on-failure
capabilities:
  docker: {}
YAML

e2e_log "registering project"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "docker-proxy-reap-on-failure" "$WS_SLUG"

task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: docker-proxy-reap-on-failure
title: Reap on failure
behavior: smoke
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

# The hook intentionally exits 1, so the task must reach aborted.
e2e_log "waiting for task aborted (hook exits 1)"
_deadline=$((SECONDS + 30))
while true; do
  _task_json="$("$E2E_BIN_DIR/boid-e2e" get-task "$task_id" 2>/dev/null)" || true
  if printf '%s' "$_task_json" | grep -q '"status":"aborted"'; then
    task_json="$_task_json"
    break
  fi
  if [[ $SECONDS -ge $_deadline ]]; then
    e2e_fail "timeout waiting for task aborted. last status: $_task_json"
  fi
  sleep 0.2
done

printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"aborted"'

# After task aborted, wait for reap to send stop+rm to the fake upstream.
e2e_log "waiting for reap: container stop"
i=0
while ! grep -qF '"method":"POST","path":"/containers/fake-c1/stop"' "$FAKE_DOCKER_LOG" 2>/dev/null; do
  sleep 0.2
  i=$((i+1))
  if [[ $i -ge 50 ]]; then
    e2e_fail "timeout: reap stop not issued to upstream after failure"
  fi
done

e2e_log "waiting for reap: container delete"
i=0
while ! grep -qF '"method":"DELETE","path":"/containers/fake-c1"' "$FAKE_DOCKER_LOG" 2>/dev/null; do
  sleep 0.2
  i=$((i+1))
  if [[ $i -ge 50 ]]; then
    e2e_fail "timeout: reap delete not issued to upstream after failure"
  fi
done

e2e_log "reap-on-failure: stop and delete confirmed in upstream log"
