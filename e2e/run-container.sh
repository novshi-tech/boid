#!/usr/bin/env bash
set -euo pipefail

# e2e/run-container.sh
#
# Container backend e2e driver (docs/plans/phase6-container-backend.md
# §PR9's "e2e-container job"). Unlike e2e/run.sh (which spins up a fresh
# `boid start` daemon directly on the runner for every scenario), this
# script drives the REAL deploy path: it builds the shared base image and
# brings up the build/container/compose.yml stack via
# scripts/deploy-container.sh, points the `boid`/`boid-e2e` CLIs at that
# compose daemon's own (bind-mounted, host-visible) socket, and dispatches
# real tasks against it — exercising the actual DooD sibling-container path
# real docker provides, which no other test in this repo does (every
# existing docker-proxy-* e2e scenario runs against a FAKE docker HTTP
# server over the userns backend, policy-only, no real data plane — see
# those scenarios' own scenario.sh header comments).
#
# What this verifies (§決定5's "sibling 疎通 3 要件", the plan doc's own
# §PR9 requirement):
#   1. job -> sibling TCP reachable (a job's own dockerproxy-created sibling
#      container, on the job's workspace network).
#   2. job -> a DIFFERENT workspace's sibling is UNREACHABLE (workspace
#      network isolation actually holds under real docker, not just a fake
#      HTTP mock).
#   3. `docker compose down` + `boid reap` leaves zero containers/networks/
#      volumes carrying this run's boid.install_id label.
#
# Requires a real docker engine (`docker compose` v2 plugin) — ubuntu-24.04
# GitHub Actions runners carry one by default; the day-to-day dev host used
# throughout this plan's development does NOT (podman only — see CLAUDE.md
# and this plan's own 前提 note), so this script is not expected to run
# there. Local exercise against podman is scripts/deploy-container.sh's own
# job (image build only in that case — see its own header comment).

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# shellcheck source=/dev/null
source "$SCRIPT_DIR/lib/common.sh"

e2e_require_cmd docker
e2e_require_cmd go
if ! docker compose version >/dev/null 2>&1; then
  e2e_fail "docker compose (v2 plugin) not found — required by scripts/deploy-container.sh"
fi

ROOT="$(mktemp -d "${TMPDIR:-/tmp}/boid-e2e-container-XXXXXX")"
KEEP_TEMP="${KEEP_TEMP:-0}"

BUILD_DIR="$ROOT/build"
mkdir -p "$BUILD_DIR"

# --- host<->container fixture-upstream reachability (see this script's own
# git-fixture section below for the full rationale) -------------------------
# "host.docker.internal" must resolve on BOTH sides: this runner's own shell
# (for the seed `git push` below, plain host process — no docker hosts-file
# magic reaches it) and the compose daemon's container (build/container/
# compose.yml's own `extra_hosts: host.docker.internal:host-gateway`
# entry, PR9). /etc/hosts already carrying this line is idempotent-safe to
# re-add; a bare grep-guard avoids duplicate entries on a re-run against a
# runner whose /etc/hosts persists across script invocations (unlikely in
# CI, but harmless either way).
if ! grep -q 'host.docker.internal' /etc/hosts 2>/dev/null; then
  echo "127.0.0.1 host.docker.internal" | sudo tee -a /etc/hosts >/dev/null
fi

UPSTREAM_PID=""
INSTALL_ID=""

dump_diagnostics() {
  printf '[e2e-container] ===== docker compose logs (daemon) =====\n' >&2
  (cd "$REPO_ROOT" && docker compose -f build/container/compose.yml logs --no-color daemon 2>&1 | tail -n 300) >&2 || true
  printf '[e2e-container] ===== docker ps -a (this install) =====\n' >&2
  docker ps -a --filter "label=boid.install_id=${INSTALL_ID}" >&2 2>&1 || true
  if [[ -f "$ROOT/task_a.json" ]]; then
    printf '[e2e-container] ===== task A (last observed) =====\n' >&2
    cat "$ROOT/task_a.json" >&2 || true
  fi
  if [[ -f "$ROOT/task_b.json" ]]; then
    printf '[e2e-container] ===== task B (last observed) =====\n' >&2
    cat "$ROOT/task_b.json" >&2 || true
  fi
}

cleanup() {
  local exit_code=$?
  if [[ $exit_code -ne 0 ]]; then
    dump_diagnostics
  fi

  if [[ -n "$UPSTREAM_PID" ]]; then
    kill "$UPSTREAM_PID" >/dev/null 2>&1 || true
    wait "$UPSTREAM_PID" 2>/dev/null || true
  fi

  # Teardown + requirement 3 verification always runs, even on an earlier
  # failure — a scenario that fails requirement 1/2 must not also leak every
  # docker resource it created (docs/plans/quality-gates.md's own "no silent
  # caps" spirit: a failed run's teardown failing too must be VISIBLE, not
  # swallowed by an early exit).
  e2e_log "tearing down compose stack"
  (cd "$REPO_ROOT" && docker compose -f build/container/compose.yml down --timeout 15) >"$ROOT/compose-down.log" 2>&1 || \
    printf '[e2e-container] WARN: docker compose down failed, see %s\n' "$ROOT/compose-down.log" >&2

  e2e_log "running boid reap"
  "$BUILD_DIR/boid" reap >"$ROOT/reap.log" 2>&1 || \
    printf '[e2e-container] WARN: boid reap failed, see %s\n' "$ROOT/reap.log" >&2
  cat "$ROOT/reap.log" >&2 || true

  if [[ -n "$INSTALL_ID" ]]; then
    local leaked
    leaked="$(docker ps -aq --filter "label=boid.install_id=${INSTALL_ID}")$(docker network ls -q --filter "label=boid.install_id=${INSTALL_ID}")$(docker volume ls -q --filter "label=boid.install_id=${INSTALL_ID}")"
    if [[ -n "$leaked" ]]; then
      printf '[e2e-container] ===== leaked resources after reap (install_id=%s) =====\n' "$INSTALL_ID" >&2
      docker ps -a --filter "label=boid.install_id=${INSTALL_ID}" >&2 || true
      docker network ls --filter "label=boid.install_id=${INSTALL_ID}" >&2 || true
      docker volume ls --filter "label=boid.install_id=${INSTALL_ID}" >&2 || true
      exit_code=1
      e2e_log "requirement 3 (reap sweeps everything) FAILED"
    else
      e2e_log "requirement 3 (reap sweeps everything) OK — zero resources remain for install_id=${INSTALL_ID}"
    fi
  fi

  if [[ $exit_code -ne 0 || $KEEP_TEMP -eq 1 ]]; then
    printf '[e2e-container] temp root preserved at %s\n' "$ROOT" >&2
  else
    rm -rf "$ROOT" >/dev/null 2>&1 || true
  fi
  exit "$exit_code"
}
trap cleanup EXIT

# --- build boid + boid-e2e --------------------------------------------------
e2e_log "building boid binary"
e2e_run go build -o "$BUILD_DIR/boid" "$REPO_ROOT"
e2e_log "building boid-e2e helper"
e2e_run go build -o "$BUILD_DIR/boid-e2e" "$REPO_ROOT/e2e/cmd/boid-e2e"
PATH="$BUILD_DIR:$PATH"
export PATH

# --- throwaway XDG layout (source == target with the compose daemon's own
# bind mounts, docs/plans/phase6-container-backend.md §決定4) ---------------
export XDG_DATA_HOME="$ROOT/data"
export XDG_CONFIG_HOME="$ROOT/config"
export XDG_RUNTIME_DIR="$ROOT/run"
mkdir -p "$XDG_DATA_HOME/boid" "$XDG_CONFIG_HOME/boid" "$XDG_RUNTIME_DIR"

# scripts/deploy-container.sh derives BOID_DATA_DIR/BOID_CONFIG_DIR/
# BOID_RUNTIME_DIR/BOID_UID/BOID_GID/DOCKER_GID from the XDG_* vars above and
# exports them for its OWN process — those exports do NOT propagate back to
# this script (it's invoked as a separate `bash` child below), so every
# later `docker compose` call this script makes directly (down/logs in the
# cleanup trap) needs the identical values computed independently here too,
# or compose's variable interpolation silently falls back to blank strings
# (harmless for `down`'s container matching, which goes by compose project
# name, but produces confusing "variable is not set" warnings and would
# matter for anything that DOES need the resolved bind-mount paths).
export BOID_DATA_DIR="$XDG_DATA_HOME/boid"
export BOID_CONFIG_DIR="$XDG_CONFIG_HOME/boid"
export BOID_RUNTIME_DIR="$XDG_RUNTIME_DIR"
export BOID_UID
BOID_UID="$(id -u)"
export BOID_GID
BOID_GID="$(id -g)"
export DOCKER_GID
DOCKER_GID="$(getent group docker 2>/dev/null | cut -d: -f3)"
: "${DOCKER_GID:=999}"

cat > "$XDG_CONFIG_HOME/boid/config.yaml" <<'YAML'
sandbox:
  backend: container
web:
  http_addr: "127.0.0.1:0"
YAML

# --- fixture git upstream (own minimal setup, NOT e2e/lib/common.sh's
# e2e_setup_fixture_upstream) ------------------------------------------------
# That shared helper always binds 127.0.0.1 (fine for every OTHER e2e
# scenario, whose daemon runs as a plain host process sharing that same
# loopback) — the compose daemon here runs inside its OWN network
# namespace, where 127.0.0.1 means the CONTAINER's loopback, not the
# runner's. This needs a 0.0.0.0-bound listener reachable from both sides
# via "host.docker.internal" (this script's own /etc/hosts line above +
# compose.yml's matching extra_hosts entry) instead — different enough from
# the shared helper's fixed 127.0.0.1 default that forking a few lines here
# is cleaner than parameterizing (and risking) code every other e2e
# scenario also depends on.
UPSTREAM_DIR="$ROOT/upstream-repos"
UPSTREAM_READY="$ROOT/upstream.addr"
UPSTREAM_CERT="$XDG_RUNTIME_DIR/upstream-ca.crt" # under BOID_RUNTIME_DIR: host-visible from inside the daemon container too
mkdir -p "$UPSTREAM_DIR"

e2e_log "starting fixture git upstream (0.0.0.0, host.docker.internal-reachable)"
"$BUILD_DIR/boid-e2e" upstream-serve \
  --dir "$UPSTREAM_DIR" \
  --addr "0.0.0.0:0" \
  --ready-file "$UPSTREAM_READY" \
  --cert-file "$UPSTREAM_CERT" \
  "e2e-fixture/proj-a" "e2e-fixture/proj-b" \
  >"$ROOT/upstream.stdout.log" 2>"$ROOT/upstream.stderr.log" &
UPSTREAM_PID=$!

e2e_wait_for_file "$UPSTREAM_READY" 10
UPSTREAM_BOUND="$(cat "$UPSTREAM_READY")"
UPSTREAM_PORT="${UPSTREAM_BOUND##*:}"
UPSTREAM_HOST="host.docker.internal:${UPSTREAM_PORT}"
e2e_log "fixture upstream bound on ${UPSTREAM_BOUND}, reachable via ${UPSTREAM_HOST}"

export SSL_CERT_FILE="$UPSTREAM_CERT"
export GIT_SSL_CAINFO="$UPSTREAM_CERT"

seed_project() {
  local dir="$1" repo="$2"
  local origin_url="https://${UPSTREAM_HOST}/e2e-fixture/${repo}.git"
  (
    cd "$dir"
    /usr/bin/git init -q -b main
    /usr/bin/git config user.name "E2E Container Fixture"
    /usr/bin/git config user.email "e2e-container-fixture@boid.test"
    /usr/bin/git add -A
    /usr/bin/git commit -q -m "e2e-container fixture seed" --allow-empty
    /usr/bin/git remote add origin "$origin_url"
    /usr/bin/git push -q -u origin HEAD
  )
}

# --- fixture projects: two workspaces, each with capabilities.docker -------
WS_ROOT="$ROOT/workspace"
PROJ_A="$WS_ROOT/proj-a"
PROJ_B="$WS_ROOT/proj-b"
mkdir -p "$PROJ_A/.boid/hooks" "$PROJ_B/.boid/hooks"

cat > "$PROJ_A/.boid/project.yaml" <<'YAML'
id: container-e2e-ws-a
name: Container E2E - workspace A
task_behaviors:
  verify:
    hooks:
      - id: verify-sibling
        command: |
          bash ".boid/hooks/verify-sibling.sh"
    name: verify
YAML

cat > "$PROJ_B/.boid/project.yaml" <<'YAML'
id: container-e2e-ws-b
name: Container E2E - workspace B
task_behaviors:
  setup:
    hooks:
      - id: setup-sibling
        command: |
          bash ".boid/hooks/setup-sibling.sh"
    name: setup
YAML

# Shared helpers both hook scripts below inline (kept duplicated rather than
# sourced — a sandboxed job's cwd is the sandbox-internal project checkout,
# which has no path back to this file).
read -r -d '' HOOK_PRELUDE <<'BASH' || true
fail_with_diag() {
  local reason="$1"
  printf '{"artifact":{"result":"fail","reason":"%s"}}\n' "$reason" | boid task update --payload-patch @- || true
  echo "FAIL: $reason" >&2
  exit 1
}

_dh="${DOCKER_HOST:-}"
[[ -n "$_dh" ]] || fail_with_diag "DOCKER_HOST is unset (per-job docker proxy did not start)"
[[ "$_dh" == unix://* ]] || fail_with_diag "DOCKER_HOST is not a unix:// path: $_dh"
DOCKER_SOCK="${_dh#unix://}"

docker_req() {
  local method="$1" path="$2" body="${3:-}"
  local -a args=(-s --max-time 15 --unix-socket "$DOCKER_SOCK" -X "$method")
  if [[ -n "$body" ]]; then
    args+=(-H 'Content-Type: application/json' -d "$body")
  fi
  curl "${args[@]}" "http://localhost${path}"
}

# Best-effort pull — the CI job also pre-pulls this image host-side (same
# docker image store, DooD) so this is defense in depth, not load-bearing.
docker_req POST "/images/create?fromImage=busybox&tag=stable" >/dev/null 2>&1 || true

create_sibling() {
  local name="$1"
  local resp
  resp="$(docker_req POST "/containers/create?name=${name}" '{"Image":"busybox:stable","Cmd":["httpd","-f","-p","8080","-h","/"]}')"
  local cid
  cid="$(printf '%s' "$resp" | grep -o '"Id":"[a-f0-9]*"' | head -1 | cut -d'"' -f4)"
  [[ -n "$cid" ]] || fail_with_diag "container create for ${name} did not return an Id: ${resp}"
  local start_code
  start_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 --unix-socket "$DOCKER_SOCK" -X POST "http://localhost/containers/${cid}/start")"
  [[ "$start_code" == "204" ]] || fail_with_diag "container start for ${name} returned HTTP ${start_code}"
  printf '%s' "$cid"
}

probe_http() {
  local url="$1" max_time="$2"
  local code
  # `|| true` (not `|| printf '000'`): curl's own -w template already
  # prints "000" on a connection/DNS failure (no HTTP response received),
  # so appending a second printf on top of curl's own already-emitted
  # "000" would concatenate into "000000" — never equal to the "000"
  # sentinel the two call sites below compare against, silently turning
  # every genuine failure into a false "reachable" result. `|| true` only
  # stops `set -e` from aborting the script on curl's nonzero exit; the
  # explicit empty-string fallback below covers the rarer case where curl
  # produced no -w output at all.
  code="$(curl -s -o /dev/null -w '%{http_code}' --max-time "$max_time" "$url" 2>/dev/null || true)"
  if [[ -z "$code" ]]; then
    code="000"
  fi
  printf '%s' "$code"
}
BASH

{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n\n%s\n\n' "$HOOK_PRELUDE"
  cat <<'BASH'
create_sibling sib-a >/dev/null

# Requirement 1: job -> own-workspace sibling must be reachable (Docker
# embedded DNS resolves "sib-a" only because THIS job container is also on
# ws-a's workspace network — internal/dispatcher/container_backend.go's
# ensureWorkspaceNetwork, PR9).
same_ws_ok=0
for _ in 1 2 3 4 5 6 7 8 9 10; do
  code="$(probe_http "http://sib-a:8080/" 2)"
  if [[ "$code" != "000" ]]; then
    same_ws_ok=1
    break
  fi
  sleep 0.5
done
[[ "$same_ws_ok" == "1" ]] || fail_with_diag "requirement 1 FAILED: sib-a (own workspace) was not reachable"

# Requirement 2: job -> a DIFFERENT workspace's sibling must be unreachable
# — "sib-b" (workspace B's sibling, set up by the concurrently-dispatched
# setup-sibling task) must not resolve/connect from ws-a's own isolated
# network at all.
cross_ws_code="$(probe_http "http://sib-b:8080/" 4)"
if [[ "$cross_ws_code" != "000" ]]; then
  fail_with_diag "requirement 2 FAILED: sib-b (different workspace) WAS reachable (http ${cross_ws_code})"
fi

printf '{"artifact":{"result":"pass"}}\n' | boid task update --payload-patch @-
BASH
} > "$PROJ_A/.boid/hooks/verify-sibling.sh"
chmod +x "$PROJ_A/.boid/hooks/verify-sibling.sh"

{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n\n%s\n\n' "$HOOK_PRELUDE"
  cat <<'BASH'
create_sibling sib-b >/dev/null

# Signal readiness BEFORE sleeping, so the CI driver can dispatch the
# verify-sibling task once sib-b actually exists and is listening.
printf '{"artifact":{"sib_ready":true}}\n' | boid task update --payload-patch @-

# Stay alive long enough for the concurrently-dispatched verify-sibling task
# to run its cross-workspace reachability check against sib-b —
# reapAndCloseDockerProxy destroys every docker resource THIS job's proxy
# created the moment this script exits (existing, intentional contract —
# see e2e/scenarios/docker-proxy-reap-on-*'s own scenario.sh), so sib-b must
# still be alive when that check runs.
sleep 30

printf '{"artifact":{"result":"pass"}}\n' | boid task update --payload-patch @-
BASH
} > "$PROJ_B/.boid/hooks/setup-sibling.sh"
chmod +x "$PROJ_B/.boid/hooks/setup-sibling.sh"

e2e_log "seeding fixture git upstream for both projects"
seed_project "$PROJ_A" "proj-a"
seed_project "$PROJ_B" "proj-b"

mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/ws-a.yaml" <<'YAML'
capabilities:
  docker: {}
YAML
cat > "$XDG_CONFIG_HOME/boid/workspaces/ws-b.yaml" <<'YAML'
capabilities:
  docker: {}
YAML

# --- pre-pull the sibling image host-side (same docker image store, DooD:
# the compose daemon and every job/sibling it creates all talk to this SAME
# host docker engine — build/container/compose.yml's own "Persistence"
# header comment) ------------------------------------------------------------
e2e_log "pre-pulling busybox:stable"
e2e_run docker pull busybox:stable

# --- build image + bring up the compose stack -------------------------------
e2e_log "building image and starting compose stack (scripts/deploy-container.sh)"
e2e_run bash "$REPO_ROOT/scripts/deploy-container.sh"

DAEMON_SOCKET="$XDG_RUNTIME_DIR/boid.sock"
e2e_log "waiting for compose daemon health at $DAEMON_SOCKET"
e2e_run "$BUILD_DIR/boid-e2e" wait-health --timeout 30s --interval 200ms "$DAEMON_SOCKET"

INSTALL_ID="$(cat "$XDG_DATA_HOME/boid/install_id" 2>/dev/null || true)"
[[ -n "$INSTALL_ID" ]] || e2e_fail "could not read install_id from $XDG_DATA_HOME/boid/install_id after daemon startup"
e2e_log "install_id=$INSTALL_ID"

e2e_log "registering projects"
e2e_run "$BUILD_DIR/boid" project add "$PROJ_A"
e2e_run "$BUILD_DIR/boid" project add "$PROJ_B"
e2e_run "$BUILD_DIR/boid" workspace assign container-e2e-ws-a ws-a
e2e_run "$BUILD_DIR/boid" workspace assign container-e2e-ws-b ws-b

create_task() {
  local project_id="$1" title="$2" behavior="$3"
  "$BUILD_DIR/boid" task create <<YAML
project_id: ${project_id}
title: ${title}
behavior: ${behavior}
YAML
}

parse_task_id() {
  sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p'
}

wait_for_marker_or_status() {
  # wait_for_marker_or_status <task_id> <out_file> <marker>
  local task_id="$1" out_file="$2" marker="$3"
  local deadline=$((SECONDS + 30))
  while true; do
    "$BUILD_DIR/boid-e2e" get-task "$task_id" >"$out_file" 2>/dev/null || true
    if grep -q "$marker" "$out_file" 2>/dev/null; then
      return 0
    fi
    if grep -q '"status":"aborted"' "$out_file" 2>/dev/null; then
      e2e_fail "task $task_id aborted while waiting for marker ${marker}: $(cat "$out_file")"
    fi
    if [[ $SECONDS -ge $deadline ]]; then
      e2e_fail "timeout waiting for marker ${marker} on task $task_id: $(cat "$out_file")"
    fi
    sleep 0.3
  done
}

wait_for_done() {
  local task_id="$1" out_file="$2"
  local deadline=$((SECONDS + 60))
  while true; do
    "$BUILD_DIR/boid-e2e" get-task "$task_id" >"$out_file" 2>/dev/null || true
    if grep -q '"status":"done"' "$out_file" 2>/dev/null; then
      return 0
    fi
    if grep -q '"status":"aborted"' "$out_file" 2>/dev/null; then
      e2e_fail "task $task_id aborted: $(cat "$out_file")"
    fi
    if [[ $SECONDS -ge $deadline ]]; then
      e2e_fail "timeout waiting for task $task_id to finish: $(cat "$out_file")"
    fi
    sleep 0.3
  done
}

e2e_log "dispatching setup-sibling (workspace B) in the background"
task_b_out="$(create_task container-e2e-ws-b "setup sibling B" setup)"
printf '%s\n' "$task_b_out"
task_b_id="$(printf '%s\n' "$task_b_out" | parse_task_id)"
[[ -n "$task_b_id" ]] || e2e_fail "failed to parse task B id from: $task_b_out"
e2e_run "$BUILD_DIR/boid" action send --task "$task_b_id" --type start

e2e_log "waiting for sib-b readiness marker"
wait_for_marker_or_status "$task_b_id" "$ROOT/task_b.json" '"sib_ready":true'
e2e_log "sib-b is ready"

e2e_log "dispatching verify-sibling (workspace A), measuring dispatch latency"
dispatch_start=$(date +%s%N)
task_a_out="$(create_task container-e2e-ws-a "verify sibling connectivity" verify)"
printf '%s\n' "$task_a_out"
task_a_id="$(printf '%s\n' "$task_a_out" | parse_task_id)"
[[ -n "$task_a_id" ]] || e2e_fail "failed to parse task A id from: $task_a_out"
e2e_run "$BUILD_DIR/boid" action send --task "$task_a_id" --type start
wait_for_done "$task_a_id" "$ROOT/task_a.json"
dispatch_end=$(date +%s%N)
dispatch_ms=$(( (dispatch_end - dispatch_start) / 1000000 ))
printf '[e2e-container][latency] task A dispatch-to-done: %sms (docker, real DooD full cycle — see docs/plans/phase6-container-backend.md §PR9 podman comparison ~150-165ms)\n' "$dispatch_ms"

task_a_json="$(cat "$ROOT/task_a.json")"
e2e_assert_contains "$task_a_json" '"status":"done"'
e2e_assert_contains "$task_a_json" '"result":"pass"'
e2e_log "requirements 1+2 (sibling reachable / cross-workspace isolated) OK"

e2e_log "waiting for setup-sibling (workspace B) to finish"
wait_for_done "$task_b_id" "$ROOT/task_b.json"
e2e_assert_contains "$(cat "$ROOT/task_b.json")" '"status":"done"'

e2e_log "container e2e scenario complete — teardown + requirement 3 verification runs in cleanup"
