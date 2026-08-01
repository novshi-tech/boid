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
# ############################################################################
# # PR-2b rewrite (docs/plans/volume-only-daemon.md §論点 e "PR 分割案 B",
# # §論点 h migration path) — this script now registers projects via the
# # NEW git-URL model instead of `boid project add <dir>`. The two gaps the
# # PREVIOUS revision's DEPRECATION NOTICE flagged are both closed here:
# #   1. install_id is no longer read from a host-visible
# #      $XDG_DATA_HOME/boid/install_id file — it's read from INSIDE the
# #      running daemon container instead (`docker compose exec`), since
# #      BOID_DATA_DIR is now the boid_state NAMED VOLUME (invisible to the
# #      host). See install_id retrieval below.
# #   2. project registration seeds a git upstream repo (already required
# #      for the sibling-container fixtures — see this script's own git-
# #      fixture section) with `.boid/project.yaml` + hook scripts COMMITTED
# #      into the repo, then registers via `boid project add <url>
# #      --workspace=<slug>` (docs/plans/volume-only-daemon.md §論点a) —
# #      the daemon clones it itself into a daemon-managed bare repo
# #      (§論点b), and PR-2b's own per-job clone (dispatcher.
# #      PrepareJobCheckout) stages a fresh per-job checkout from that bare
# #      repo straight into the job container, mirroring project.yaml's own
# #      committed .boid/hooks/*.sh into the sandbox exactly where task
# #      dispatch expects to find them.
# #
# # A THIRD gap this same pivot introduced (not covered by the previous
# # revision's notice, since it never got far enough to hit it):
# # BOID_CONFIG_DIR is ALSO now inside the boid_state named volume, so this
# # script can no longer just write $XDG_CONFIG_HOME/boid/config.yaml on the
# # HOST and expect the compose daemon to see it (that bind mount was
# # retired alongside BOID_DATA_DIR's — see compose.yml's own "Persistence"
# # header comment). config.yaml (specifically `sandbox.backend: container`,
# # which the daemon reads once at startup, before any `boid config apply`
# # RPC could possibly reach it) is instead SEEDED into the fresh volume via
# # a one-off `docker compose run` BEFORE the real daemon's first boot — see
# # the "seed config.yaml" step below. `scripts/deploy-container.sh`'s own
# # trailing echo ("opt in per-host with 'sandbox: {backend: container}' in
# # ~/.config/boid/config.yaml") is stale in the same way and worth fixing
# # in a follow-up, but this script does not depend on it.
# ############################################################################
#
# What this verifies (§決定5's "sibling 疎通 3 要件", the plan doc's own
# §PR9 requirement):
#   1. job -> sibling reachable at the address §決定5 promises — container IP
#      + container port, dialed DIRECTLY (a job's own dockerproxy-created
#      sibling container, on the job's workspace network). Reaching it
#      requires the job's own workspace subnet to be in no_proxy, else curl
#      hands the request to the egress proxy, which refuses to relay into any
#      workspace network. The same sibling addressed by its single-label
#      container NAME must still be refused (1b) — that refusal is what
#      requirement 2 rests on.
#   2. job -> a DIFFERENT workspace's sibling is UNREACHABLE (workspace
#      network isolation actually holds under real docker, not just a fake
#      HTTP mock).
#   3. `docker compose down` + `boid reap` leaves zero containers/networks/
#      volumes belonging to this run — both the ones carrying its
#      boid.install_id label AND the per-workspace HOME volumes, which
#      deliberately carry boid.workspace_home_install_id INSTEAD (see the
#      teardown below).
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
# entry, PR9) — the daemon container is ALSO where project registration's
# own `git clone` now runs (PR-2b: the daemon clones a project's git-URL
# itself, docs/plans/volume-only-daemon.md §論点a/b), so this reachability
# requirement is unchanged from the previous revision, just serving one
# more caller.
if ! grep -q 'host.docker.internal' /etc/hosts 2>/dev/null; then
  echo "127.0.0.1 host.docker.internal" | sudo tee -a /etc/hosts >/dev/null
fi

UPSTREAM_PID=""
INSTALL_ID=""

dump_diagnostics() {
  # docker compose logs (and plain `docker logs`) show NOTHING for the
  # daemon service by design (compose.yml's own header comment on the
  # daemon service: runDaemonChild redirects stdin/stdout/stderr to
  # boid.log as its first action, before anything else could print) — kept
  # here anyway as a zero-cost first check in case a failure happens
  # BEFORE that redirect (image/entrypoint problems, a crash in flag
  # parsing, ...), but boid.log itself (XDG_STATE_HOME, PR9) is the
  # primary source below.
  printf '[e2e-container] ===== docker compose logs (daemon) =====\n' >&2
  (cd "$REPO_ROOT" && docker compose -f build/container/compose.yml logs --no-color daemon 2>&1 | tail -n 300) >&2 || true

  # The daemon container itself carries no boid.install_id label (only
  # containerBackend-created job/sibling resources do — see
  # container_backend.go's labelInstallID) — filtering by that label here
  # would always show zero rows for the daemon's own container/state.
  printf '[e2e-container] ===== docker compose ps -a (this stack) =====\n' >&2
  (cd "$REPO_ROOT" && docker compose -f build/container/compose.yml ps -a 2>&1) >&2 || true
  printf '[e2e-container] ===== docker inspect (daemon container State) =====\n' >&2
  (cd "$REPO_ROOT" && docker inspect "$(docker compose -f build/container/compose.yml ps -aq daemon 2>/dev/null)" --format '{{json .State}}' 2>&1) >&2 || true

  # boid.log itself (PR9's XDG_STATE_HOME fix, compose.yml): lands at
  # $XDG_RUNTIME_DIR/boid/boid.log on the HOST (daemon.LogFilePath() =
  # $XDG_STATE_HOME/boid/boid.log, and XDG_STATE_HOME is set to
  # BOID_RUNTIME_DIR — the same bind-mounted dir XDG_RUNTIME_DIR points
  # at), so it is readable directly off disk without a live `docker exec`
  # and survives the container's own teardown below. This compose deploy
  # additionally sets BOID_LOG_STDOUT=1, so in practice the daemon's own
  # slog output goes to stdout/stderr (captured by "docker compose logs"
  # above) and this file is never created — kept as a fallback in case a
  # future deploy unsets that.
  local daemon_log="$XDG_RUNTIME_DIR/boid/boid.log"
  if [[ -f "$daemon_log" ]]; then
    printf '[e2e-container] ===== %s (tail) =====\n' "$daemon_log" >&2
    tail -n 300 "$daemon_log" >&2 || true
  else
    printf '[e2e-container] ===== %s does not exist =====\n' "$daemon_log" >&2
  fi

  printf '[e2e-container] ===== docker ps -a (this install, job/sibling containers) =====\n' >&2
  docker ps -a --filter "label=boid.install_id=${INSTALL_ID}" >&2 2>&1 || true

  # Every job's own runner-state.json diagnostic + transcript.log (the
  # actual hook stdout/stderr the container-backend attach stream spooled
  # to disk, §決定8) now live under the HOST-VISIBLE
  # $XDG_RUNTIME_DIR/runtimes/ root — internal/server/wire.go's
  # hostVisibleRuntimesDirFor(cfg) (PR-2b), not the previous revision's
  # $XDG_DATA_HOME/boid/runtimes/ (that root is inside the boid_state named
  # volume post-pivot, invisible to the host — see that function's own doc
  # comment for the full rationale). transcript.log is keyed by the
  # container ID (containerBackend.openTranscriptSpool), runner-state.json
  # by job ID under a "spec/" subdirectory (containerBackend.
  # writeContainerSpec) — both globs below cover their own layout.
  local runtimes_dir="$XDG_RUNTIME_DIR/runtimes"
  if [[ -d "$runtimes_dir" ]]; then
    local f
    for f in "$runtimes_dir"/*/transcript.log; do
      [[ -f "$f" ]] || continue
      printf '[e2e-container] ===== %s =====\n' "$f" >&2
      cat "$f" >&2 || true
    done
    for f in "$runtimes_dir"/spec/*/runner-state.json; do
      [[ -f "$f" ]] || continue
      printf '[e2e-container] ===== %s =====\n' "$f" >&2
      cat "$f" >&2 || true
    done
  fi

  if [[ -f "$ROOT/task_a.json" ]]; then
    printf '[e2e-container] ===== task A (last observed) =====\n' >&2
    cat "$ROOT/task_a.json" >&2 || true
  fi
  if [[ -f "$ROOT/task_b.json" ]]; then
    printf '[e2e-container] ===== task B (last observed) =====\n' >&2
    cat "$ROOT/task_b.json" >&2 || true
  fi
  if [[ -f "$ROOT/task_c.json" ]]; then
    printf '[e2e-container] ===== task C (broker TLS, last observed) =====\n' >&2
    cat "$ROOT/task_c.json" >&2 || true
  fi
  if [[ -f "$ROOT/task_c2.json" ]]; then
    printf '[e2e-container] ===== task C2 (git push, last observed) =====\n' >&2
    cat "$ROOT/task_c2.json" >&2 || true
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
  #
  # --volumes (codex round-1, PR834 Major 3): without it, the fixed-name
  # `boid_state` named volume (build/container/compose.yml's own
  # `volumes:` section — NOT a per-run-random name; compose derives its
  # name from the project name, which this script's every invocation
  # resolves identically) survives this teardown and is reused verbatim by
  # the NEXT run's `up`. That next run's own config-seed step (this
  # script's own "seed config.yaml" step above) only ever touches
  # config.yaml — boid.db (and everything else this run wrote into
  # boid_state: workspace/project rows, secret material, ...) carries over
  # untouched, so the next run's workspace/project creation calls collide
  # with this run's own leftover rows instead of starting from a genuinely
  # fresh daemon. `--volumes` removes the named volume along with the
  # stack, restoring the fresh-daemon precondition every invocation of this
  # script actually depends on (and that a persistent, non-CI runner would
  # otherwise violate on its second and every subsequent run).
  e2e_log "tearing down compose stack (including named volumes)"
  (cd "$REPO_ROOT" && docker compose -f build/container/compose.yml down --volumes --timeout 15) >"$ROOT/compose-down.log" 2>&1 || \
    printf '[e2e-container] WARN: docker compose down failed, see %s\n' "$ROOT/compose-down.log" >&2

  if [[ -n "$INSTALL_ID" ]]; then
    # --install-id (cmd/reap.go): reap this specific install_id via the
    # docker engine's own label-based queries, without needing to read
    # ~/.local/share/boid/install_id off THIS process's local filesystem —
    # which, for a compose deploy, is a different filesystem than the
    # daemon container's own boid_state volume entirely (PR-2b; see this
    # script's own header comment). INSTALL_ID was already captured from
    # INSIDE the running daemon container, before the `down` above.
    #
    # --include-workspace-homes (PR6 of
    # docs/plans/workspace-home-volume-persistence.md): a workspace HOME volume
    # is PERSISTENT by design and carries neither boid.install_id nor
    # boid.job_id — being invisible to every sweep is the whole point of 論点
    # a's label decision, since a daemon restart must never take a workspace's
    # credentials and toolchain with it. Three consequences meet here, and only
    # the flag resolves them:
    #
    #   - `docker compose down --volumes` removes only volumes DECLARED in
    #     compose.yml, so it does not touch these (their names are computed at
    #     run time from the install id).
    #   - a plain `boid reap --install-id` deliberately skips them, and says so
    #     in its output.
    #   - the leak check below enumerates by label, and boid.install_id is not
    #     one they carry.
    #
    # So without this, every e2e run that dispatches a job — which is every run,
    # the sibling-connectivity requirements are hook jobs — would leave one
    # boid-ws-home-* volume per workspace behind on the CI host, forever, with
    # requirement 3 reporting success. The install id is fresh per run, so they
    # would not even be reused.
    e2e_log "running boid reap --install-id ${INSTALL_ID} --include-workspace-homes"
    "$BUILD_DIR/boid" reap --install-id "$INSTALL_ID" --include-workspace-homes >"$ROOT/reap.log" 2>&1 || \
      printf '[e2e-container] WARN: boid reap failed, see %s\n' "$ROOT/reap.log" >&2
    cat "$ROOT/reap.log" >&2 || true

    # Both label families, because they name disjoint sets: boid.install_id
    # covers the job containers/networks/volumes, boid.workspace_home_install_id
    # covers the persistent HOME volumes. Checking only the first is what would
    # let the leak above go unnoticed.
    local leaked
    leaked="$(docker ps -aq --filter "label=boid.install_id=${INSTALL_ID}")$(docker network ls -q --filter "label=boid.install_id=${INSTALL_ID}")$(docker volume ls -q --filter "label=boid.install_id=${INSTALL_ID}")$(docker volume ls -q --filter "label=boid.workspace_home_install_id=${INSTALL_ID}")"
    if [[ -n "$leaked" ]]; then
      printf '[e2e-container] ===== leaked resources after reap (install_id=%s) =====\n' "$INSTALL_ID" >&2
      docker ps -a --filter "label=boid.install_id=${INSTALL_ID}" >&2 || true
      docker network ls --filter "label=boid.install_id=${INSTALL_ID}" >&2 || true
      docker volume ls --filter "label=boid.install_id=${INSTALL_ID}" >&2 || true
      docker volume ls --filter "label=boid.workspace_home_install_id=${INSTALL_ID}" >&2 || true
      exit_code=1
      e2e_log "requirement 3 (reap sweeps everything) FAILED"
    else
      e2e_log "requirement 3 (reap sweeps everything) OK — zero resources remain for install_id=${INSTALL_ID}"
    fi
  else
    printf '[e2e-container] WARN: INSTALL_ID was never captured; skipping reap + requirement 3 verification\n' >&2
    exit_code=1
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

# --- throwaway XDG layout ----------------------------------------------------
# XDG_DATA_HOME/XDG_CONFIG_HOME below are still exported for THIS script's
# own CLI invocations to resolve a consistent, disposable per-run
# ~/.local/share and ~/.config (e.g. boid-e2e's own state, and
# workspaceCreateFromFile temp yaml below) — they are NOT bind-mounted into
# the compose daemon any more (PR-2b; BOID_DATA_DIR/BOID_CONFIG_DIR are the
# boid_state NAMED VOLUME now, source==target host bind mounts for them were
# retired by the volume-only pivot — see compose.yml's own "Persistence"
# header comment). Only XDG_RUNTIME_DIR (-> BOID_RUNTIME_DIR) is still a
# real, load-bearing host<->container bind (the daemon socket, boid.log,
# and — as of PR-2b — the per-job DooD material / job-log read path all
# live under it; see hostVisibleRuntimesDirFor's own doc comment).
export XDG_DATA_HOME="$ROOT/data"
export XDG_CONFIG_HOME="$ROOT/config"
export XDG_RUNTIME_DIR="$ROOT/run"
mkdir -p "$XDG_DATA_HOME/boid" "$XDG_CONFIG_HOME/boid" "$XDG_RUNTIME_DIR"

# --- BOID_CLI_TOKEN (PR-3 Option 4 host-mode redesign, docs/plans/
# volume-only-daemon.md §論点c, nose directive 2026-07-25) -------------------
# Pre-seeded into $XDG_CONFIG_HOME/boid/cli-token BEFORE the daemon ever
# boots (below) so a LATER `BOID_MODE=container "$BUILD_DIR/boid" ...`
# invocation's own loadOrCreateCLIToken (cmd/host.go) reads back this exact
# value (atomicfile.PublishIfAbsent: an existing file is read back, not
# regenerated) instead of racing its own fresh one — the two sides
# (this script's `export BOID_CLI_TOKEN=...` below, consumed by
# scripts/deploy-container.sh's `compose up` at the bottom of this script,
# and cmd/host.go's own token file read) must agree on the SAME secret for
# host-mode CLI dispatch (verify-cli-token-listener below) to authenticate
# at all.
CLI_TOKEN_FILE="$XDG_CONFIG_HOME/boid/cli-token"
CLI_TOKEN="$(head -c32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
printf '%s' "$CLI_TOKEN" > "$CLI_TOKEN_FILE"
chmod 600 "$CLI_TOKEN_FILE"
export BOID_CLI_TOKEN="$CLI_TOKEN"

# scripts/deploy-container.sh derives BOID_RUNTIME_DIR/BOID_UID/BOID_GID/
# DOCKER_GID from XDG_RUNTIME_DIR/id -u/id -g/getent — mirrored here
# independently (same values, computed the same way) so every later
# `docker compose` call THIS script makes directly (the config.yaml seed
# run below, and down/logs/exec in the cleanup trap and diagnostics) agrees
# with what deploy-container.sh's own subsequent `up` resolves, or
# compose's variable interpolation would fall back to blank strings /
# resolve a DIFFERENT boid_state volume identity for different calls
# (compose derives its project/volume naming from, among other things,
# these variables' resolved values reaching the same docker Compose
# project consistently is what keeps every invocation in this script
# talking to the SAME stack).
export BOID_RUNTIME_DIR="$XDG_RUNTIME_DIR"
export BOID_UID
BOID_UID="$(id -u)"
export BOID_GID
BOID_GID="$(id -g)"
export DOCKER_GID
DOCKER_GID="$(getent group docker 2>/dev/null | cut -d: -f3)"
: "${DOCKER_GID:=999}"

# --- build the boid-runner image FIRST (codex round-1, PR834 Blocker 2) -----
# The config-seed step immediately below runs `docker compose run ... daemon`
# — and build/container/compose.yml's `daemon` service has no `build:`
# section, only `image: boid-runner:latest` — so on a fresh runner with no
# LOCAL boid-runner:latest image yet (true of every CI runner before this
# script has ever built one), that `run` would try to PULL a nonexistent/
# private image and fail before configuration (or anything else) ever gets a
# chance to run. This previously only happened much later, inside
# scripts/deploy-container.sh's own call at the bottom of this script — too
# late for the config-seed step above it. DEPLOY_CONTAINER_BUILD_ONLY=1 runs
# just that script's build step, standalone, so the image exists before this
# script's own FIRST `docker compose` call of any kind; deploy-container.sh's
# own later, unconditional (full build+seed+up) invocation re-runs the same
# build, cheaply, thanks to docker/podman's own layer cache.
e2e_log "building the boid-runner image (before config seed, so it has an image to run against)"
DEPLOY_CONTAINER_BUILD_ONLY=1 e2e_run bash "$REPO_ROOT/scripts/deploy-container.sh"

# --- seed config.yaml into the (fresh) boid_state volume --------------------
# web.http_addr must be in place BEFORE the daemon's own first boot
# (internal/server/wire.go's buildRuntime reads it once, via config.Load(),
# long before any `boid config apply` RPC could possibly reach a
# not-yet-running daemon) — and BOID_CONFIG_DIR is the boid_state NAMED
# VOLUME as of the volume-only pivot, so simply writing
# $XDG_CONFIG_HOME/boid/config.yaml on THIS script's own host filesystem
# (what the previous revision did, back when that path was a host bind
# mount) no longer reaches the container at all. `docker compose run` with
# an overridden entrypoint creates a ONE-OFF container from the exact same
# service definition (same image, same boid_state volume mount, same
# `user: "${BOID_UID}:0"` — arbitrary-uid, docs/plans/
# release-onboarding.md 決定1/PR2) purely to write the file, then exits — the
# subsequent `up` (scripts/deploy-container.sh, below) starts the real,
# long-running daemon against that now-seeded volume. (The image itself is
# already guaranteed to exist by the build step immediately above.)
#
# sandbox.backend: container is no longer seeded here (PR-4, docs/plans/
# volume-only-daemon.md §論点e) — container is the only sandbox backend
# now, selected unconditionally by internal/server/wire.go's buildRuntime,
# not by config.
e2e_log "seeding config.yaml into the boid_state volume (web.http_addr)"
(cd "$REPO_ROOT" && docker compose -f build/container/compose.yml run --rm -T --entrypoint sh daemon -c \
  'mkdir -p "$XDG_CONFIG_HOME/boid" && cat > "$XDG_CONFIG_HOME/boid/config.yaml"') <<'YAML'
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
  "e2e-fixture/proj-a" "e2e-fixture/proj-b" "e2e-fixture/proj-c" \
  >"$ROOT/upstream.stdout.log" 2>"$ROOT/upstream.stderr.log" &
UPSTREAM_PID=$!

e2e_wait_for_file "$UPSTREAM_READY" 10
UPSTREAM_BOUND="$(cat "$UPSTREAM_READY")"
UPSTREAM_PORT="${UPSTREAM_BOUND##*:}"
UPSTREAM_HOST="host.docker.internal:${UPSTREAM_PORT}"
e2e_log "fixture upstream bound on ${UPSTREAM_BOUND}, reachable via ${UPSTREAM_HOST}"

export SSL_CERT_FILE="$UPSTREAM_CERT"
export GIT_SSL_CAINFO="$UPSTREAM_CERT"

# --- fixture project sources: PR-2b registers via git URL, so
# .boid/project.yaml + hooks now live IN THE REPO ITSELF (committed and
# pushed, below), not in a directory `boid project add <dir>` would read
# host-side. FIXTURE_ROOT can be anywhere under $ROOT — unlike the previous
# revision's WS_ROOT, it no longer needs to sit under
# $XDG_DATA_HOME/boid/... (that constraint was specific to the retired
# host-directory registration path, whose comment explained the daemon
# reads work_dir off ITS OWN filesystem view — irrelevant now that the
# daemon does its own git clone instead).
FIXTURE_ROOT="$ROOT/fixture-src"
PROJ_A="$FIXTURE_ROOT/proj-a"
PROJ_B="$FIXTURE_ROOT/proj-b"
PROJ_C="$FIXTURE_ROOT/proj-c"
mkdir -p "$PROJ_A/.boid/hooks" "$PROJ_B/.boid/hooks" "$PROJ_C/.boid/hooks"

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

# Third fixture project — no capabilities.docker, no sibling containers:
# this one exists solely to pin docs/plans/phase6-cutover-followups.md §⓪
# ("broker TCP wire completion") itself, distinct from proj-a/proj-b's own
# sibling-connectivity requirements 1/2. See verify-broker-tls.sh below.
#
# git-push (codex round-1, PR834 Minor 3): a second, independent behavior on
# the SAME project — exercises `git push` from inside a job container,
# something neither the verify-broker-tls task above nor proj-a/proj-b's own
# sibling-reachability tasks ever touch. Covers two gaps together:
#   1. remote.origin.url was actually rewritten to the gateway clone URL
#      (dispatcher.PrepareJobCheckout's own remoteURL step), not left
#      pointing at the daemon-local bare-repo filesystem path a job
#      container cannot even see.
#   2. the per-job clone's own object store is a genuine, standalone
#      COPY, not a `--reference` alternates pointer back at the daemon's
#      bare repo (codex round-1 Blocker 1, internal/dispatcher/checkout.go)
#      — `git commit` (a HEAD-touching operation exactly like the alternates
#      failure mode described in that fix) must succeed for this task's
#      own commit-then-push sequence to get anywhere at all.
cat > "$PROJ_C/.boid/project.yaml" <<'YAML'
id: container-e2e-ws-c
name: Container E2E - workspace C (broker TLS)
task_behaviors:
  verify:
    hooks:
      - id: verify-broker-tls
        command: |
          bash ".boid/hooks/verify-broker-tls.sh"
    name: verify
  git-push:
    # readonly: false (orchestrator's own TaskBehavior.Readonly doc comment:
    # fail-safe default is readonly=true for every behavior except the
    # canonical "executor") — this task's own hook needs a writable sandbox
    # to add push-probe.txt, `git commit`, and `git push`.
    readonly: false
    hooks:
      - id: verify-git-push
        command: |
          bash ".boid/hooks/verify-git-push.sh"
    name: git-push
YAML

# Shared helpers both hook scripts below inline (kept duplicated rather than
# sourced — a sandboxed job's cwd is the sandbox-internal project checkout,
# which has no path back to this file).
read -r -d '' HOOK_PRELUDE <<'BASH' || true
fail_with_diag() {
  local reason="$1"
  printf '{"artifact":{"result":"fail","reason":"%s"}}\n' "$reason" | boid task update --payload-patch @- >/dev/null 2>&1 || true
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

sibling_ip() {
  local cid="$1" resp ip
  resp="$(docker_req GET "/containers/${cid}/json")"
  # The inspect body carries an IPAddress at BOTH the legacy top level of
  # NetworkSettings (empty for a container on a user-defined network — every
  # sibling here) and inside NetworkSettings.Networks.<name>. Requiring the
  # value to START with a digit is what skips the empty legacy one; `head -1`
  # alone would happily return "".
  ip="$(printf '%s' "$resp" | grep -o '"IPAddress":"[0-9][0-9.]*"' | head -1 | cut -d'"' -f4)"
  [[ -n "$ip" ]] || fail_with_diag "could not read container IP for sibling ${cid} out of its inspect body"
  printf '%s' "$ip"
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
sib_a_cid="$(create_sibling sib-a)"
sib_a_ip="$(sibling_ip "$sib_a_cid")"

# Requirement 1: job -> own-workspace sibling must be reachable at the
# address §決定5 actually promises — "container IP + container port の直
# アクセス". The TCP route exists because THIS job container is also on ws-a's
# workspace network (internal/dispatcher/container_backend.go's
# ensureWorkspaceNetwork, PR9), but curl only DIALS that route directly if the
# address bypasses HTTP_PROXY: the egress proxy lives in the daemon container
# and deliberately refuses to relay into any workspace network (internal/
# sandbox/proxy.go's isRefusedDotlessTarget doc comment — relaying there would
# break requirement 2, since the daemon is joined to every active workspace
# network at once). That bypass is applyProxyEnv putting the job's OWN
# workspace subnet in no_proxy, resolved per job via
# containerBackend.WorkspaceNetworkCIDRs.
#
# Note what this loop does NOT accept, and why the previous revision's
# `[[ "$code" != "000" ]]` was a false pass: the egress proxy answers a
# proxied request for an unlisted target with 403 (dotted/IP host) or 502
# (single-label host). Both are non-"000", so the old check reported
# "reachable" for a request that never left the daemon — and it did exactly
# that for the entire pre-fix window, since no_proxy carried no workspace
# subnet at all. 403/502 are therefore hard failures here, not retries that
# eventually give up.
same_ws_code=""
for _ in 1 2 3 4 5 6 7 8 9 10; do
  same_ws_code="$(probe_http "http://${sib_a_ip}:8080/" 2)"
  # 000 alone is worth retrying: the container is started but busybox httpd
  # may not have bound its listener yet.
  [[ "$same_ws_code" == "000" ]] || break
  sleep 0.5
done
case "$same_ws_code" in
000)
  fail_with_diag "requirement 1 FAILED: sib-a (own workspace) never answered at ${sib_a_ip}:8080 — no HTTP response at all"
  ;;
403 | 502)
  fail_with_diag "requirement 1 FAILED: the request to sib-a (${sib_a_ip}:8080) was routed through the egress proxy and refused there (http ${same_ws_code}) — this job's own workspace subnet is missing from no_proxy/NO_PROXY"
  ;;
esac

# Requirement 1b: the SAME sibling addressed by its single-label container
# name must still be refused by the egress proxy (502). This is not a
# limitation being documented — it is the isolation half of requirement 2
# being pinned from the reachable side: a bare hostname is exactly what a
# cross-workspace target looks like, the daemon's own aggregated DNS view CAN
# resolve another workspace's sibling name, and isRefusedDotlessTarget is what
# stops the relay before the dial. The direct route above is the supported way
# to reach a sibling; if a future change ever makes sibling NAMES routable
# (e.g. by giving them a dotted alias plus a no_proxy suffix), this assertion
# is the one that must be revisited deliberately rather than silently.
name_code="$(probe_http "http://sib-a:8080/" 4)"
if [[ "$name_code" != "502" ]]; then
  fail_with_diag "requirement 1b FAILED: sib-a by container NAME returned http ${name_code}, expected 502 (the egress proxy's dotless refusal). A 200/404 here means single-label targets are being relayed into workspace networks, which is the hole requirement 2 depends on being closed"
fi

# Requirement 2: job -> a DIFFERENT workspace's sibling must be unreachable
# — "sib-b" (workspace B's sibling, set up by the concurrently-dispatched
# setup-sibling task) must not resolve/connect from ws-a's own isolated
# network at all.
#
# The per-workspace subnet no_proxy entry requirement 1 now depends on does
# not widen this: no_proxy carries THIS job's own workspace subnet only
# (containerBackend.WorkspaceNetworkCIDRs), so an address on workspace B's
# network still matches nothing there, still goes to the egress proxy, and is
# still refused by it. A job also has no way to learn sib-b's IP in the first
# place — the per-job dockerproxy ledger answers 404 for resource IDs it does
# not own (internal/sandbox/dockerproxy/server.go's scope check).
#
# Note also that "502" below no longer does double duty: before requirement 1
# was addressed by container IP, the same 502 counted as PROOF OF REACH there
# and PROOF OF ISOLATION here, so both requirements passed simultaneously on
# one refusal.
#
# Two acceptable outcomes here (§⓪-b in docs/plans/phase6-cutover-followups.md):
#   * "000" — the direct network layer refused the request outright (DNS
#     NXDOMAIN or TCP unreachable). This is what a job would see if the
#     request never touched HTTP_PROXY at all (e.g. NO_PROXY covers sib-b
#     directly).
#   * "502" — HTTP_PROXY forwarded the request, and the boid egress proxy
#     (internal/sandbox/proxy.go's isRefusedDotlessTarget) refused to relay
#     it BEFORE the allowlist check. This is the load-bearing case: the
#     daemon container is self-connected to every currently-active
#     workspace network at once (§決定5's daemon-hosted broker/gateway/
#     egress-proxy reachability contract, containerBackend.
#     ensureWorkspaceNetwork), so its own DNS view can resolve "sib-b" from
#     workspace B's network even for a workspace A job — the dotless
#     refusal is the network-layer isolation contract the allowlist alone
#     does not provide (an operator with `allowed_domains: ["sib-b"]` on
#     workspace A's config would otherwise let a workspace A job reach a
#     workspace B sibling).
# Anything else — notably "403" (the allowlist rejected AFTER a
# resolution/dial succeeded, the pre-fix behavior §⓪-b was written up
# from) — is a genuine failure of requirement 2.
cross_ws_code="$(probe_http "http://sib-b:8080/" 4)"
if [[ "$cross_ws_code" != "000" && "$cross_ws_code" != "502" ]]; then
  fail_with_diag "requirement 2 FAILED: sib-b (different workspace) WAS reachable (http ${cross_ws_code}; expected 000 direct-fail or 502 dotless-refused)"
fi

printf '{"artifact":{"result":"pass"}}\n' | boid task update --payload-patch @- >/dev/null 2>&1
BASH
} > "$PROJ_A/.boid/hooks/verify-sibling.sh"
chmod +x "$PROJ_A/.boid/hooks/verify-sibling.sh"

{
  printf '#!/usr/bin/env bash\nset -euo pipefail\n\n%s\n\n' "$HOOK_PRELUDE"
  cat <<'BASH'
create_sibling sib-b >/dev/null

# Signal readiness BEFORE sleeping, so the CI driver can dispatch the
# verify-sibling task once sib-b actually exists and is listening.
printf '{"artifact":{"sib_ready":true}}\n' | boid task update --payload-patch @- >/dev/null 2>&1

# Stay alive long enough for the concurrently-dispatched verify-sibling task
# to run its cross-workspace reachability check against sib-b —
# reapAndCloseDockerProxy destroys every docker resource THIS job's proxy
# created the moment this script exits (existing, intentional contract —
# see e2e/scenarios/docker-proxy-reap-on-*'s own scenario.sh), so sib-b must
# still be alive when that check runs.
sleep 30

printf '{"artifact":{"result":"pass"}}\n' | boid task update --payload-patch @- >/dev/null 2>&1
BASH
} > "$PROJ_B/.boid/hooks/setup-sibling.sh"
chmod +x "$PROJ_B/.boid/hooks/setup-sibling.sh"

# verify-broker-tls.sh (docs/plans/phase6-cutover-followups.md §⓪): does NOT
# source $HOOK_PRELUDE — it needs no DOCKER_HOST/dockerproxy machinery at
# all, only the broker RPC path itself. Two things are pinned:
#   1. BOID_BROKER_TLS_ADDR (and its four siblings) actually reached this
#      job's env — proves internal/dispatcher/container_backend.go's
#      withBrokerTLSEnv delivery happened, not just that SOME transport
#      happened to work.
#   2. `boid task update --payload-patch @-` itself succeeds — this IS the
#      broker RPC round trip end to end (job -> TCP+mTLS dial -> broker ->
#      daemon -> task row updated): if the TLS handshake, the per-job
#      client cert, or the daemon-side mTLS verification were broken, this
#      command would fail outright (non-zero exit, `set -e` aborts the
#      script) before ever reaching the final print below.
cat > "$PROJ_C/.boid/hooks/verify-broker-tls.sh" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail

fail_with_diag() {
  local reason="$1"
  printf '{"artifact":{"result":"fail","reason":"%s"}}\n' "$reason" | boid task update --payload-patch @- >/dev/null 2>&1 || true
  echo "FAIL: $reason" >&2
  exit 1
}

for var in BOID_BROKER_TLS_ADDR BOID_BROKER_TLS_CERT_PATH BOID_BROKER_TLS_KEY_PATH BOID_BROKER_TLS_CA_PATH BOID_BROKER_TLS_SERVER_NAME; do
  [[ -n "${!var:-}" ]] || fail_with_diag "${var} is unset (broker TCP wire did not deliver into this job's env)"
done
for f in "$BOID_BROKER_TLS_CERT_PATH" "$BOID_BROKER_TLS_KEY_PATH" "$BOID_BROKER_TLS_CA_PATH"; do
  [[ -f "$f" ]] || fail_with_diag "broker TLS material file does not exist: ${f}"
done

printf '{"artifact":{"result":"pass","broker_transport":"tls","broker_tls_addr":"%s"}}\n' "$BOID_BROKER_TLS_ADDR" | boid task update --payload-patch @-
BASH
chmod +x "$PROJ_C/.boid/hooks/verify-broker-tls.sh"

# verify-git-push.sh (codex round-1, PR834 Minor 3): the job's own cwd is
# already the PR-2b per-job clone staging checkout (dispatcher.
# PrepareJobCheckout) — no separate clone/setup needed here. Two things are
# pinned, both load-bearing preconditions the rest of this project.yaml's
# tasks never happen to exercise:
#   1. remote.origin.url actually got rewritten to the gateway clone URL
#      ("/j/<token>/..." — dispatcher.buildGatewayCloneURL's own shape),
#      not left pointing at the daemon-local bare-repo path.
#   2. `git commit` + `git push origin HEAD` both succeed standalone — the
#      concrete, load-bearing consequence of codex round-1 Blocker 1's fix
#      (checkout.go's `--reference` alternates dependency): a job container
#      never mounts the daemon's own bare-repo path, so an alternates-based
#      clone's `git commit` (which touches HEAD's object graph) would have
#      failed resolving objects through a path this container cannot see.
# The runner script itself (below, after this task completes) verifies the
# push landed by reading the fixture upstream's bare repo directly off the
# HOST filesystem (the strongest possible check — independent of this job's
# own self-reported "pass").
cat > "$PROJ_C/.boid/hooks/verify-git-push.sh" <<'BASH'
#!/usr/bin/env bash
set -euo pipefail

fail_with_diag() {
  local reason="$1"
  printf '{"artifact":{"result":"fail","reason":"%s"}}\n' "$reason" | boid task update --payload-patch @- >/dev/null 2>&1 || true
  echo "FAIL: $reason" >&2
  exit 1
}

origin_url="$(git remote get-url origin 2>&1)" || fail_with_diag "git remote get-url origin failed: ${origin_url:-<no output>}"
case "$origin_url" in
  */j/*) ;;
  *) fail_with_diag "remote.origin.url does not look like a gateway clone URL (no /j/<token>/ segment): ${origin_url}" ;;
esac

marker="e2e-container git push probe ${BOID_TASK_ID:-unknown}"
printf '%s\n' "$marker" >> push-probe.txt
git add push-probe.txt
git -c user.email="e2e-container-job@boid.test" -c user.name="E2E Container Job" commit -q -m "$marker" \
  || fail_with_diag "git commit failed (per-job clone's object store may still be alternates-dependent — see checkout.go's own doc comment)"
git push -q origin HEAD || fail_with_diag "git push origin HEAD failed"

printf '{"artifact":{"result":"pass","remote_origin_url":"%s","push_marker":"%s"}}\n' "$origin_url" "$marker" | boid task update --payload-patch @-
BASH
chmod +x "$PROJ_C/.boid/hooks/verify-git-push.sh"

# --- seed fixture git upstream: commit .boid/project.yaml + hooks INTO the
# repo (PR-2b) and push, so the daemon's own `boid project add <url>`
# clone (below) carries them straight into its bare repo's HEAD tree —
# ReadProjectMetaFromBareRepo (internal/orchestrator/project_bare_repo.go)
# reads project.yaml via `git show HEAD:.boid/project.yaml`, and PR-2b's
# per-job clone (dispatcher.PrepareJobCheckout) carries .boid/hooks/*.sh
# into each job's own staging area the same way a real `git clone` would.
seed_project() {
  local dir="$1" repo="$2"
  local origin_url="https://${UPSTREAM_HOST}/e2e-fixture/${repo}.git"
  (
    cd "$dir"
    /usr/bin/git init -q -b main
    /usr/bin/git config user.name "E2E Container Fixture"
    /usr/bin/git config user.email "e2e-container-fixture@boid.test"
    /usr/bin/git add -A
    /usr/bin/git commit -q -m "e2e-container fixture seed"
    /usr/bin/git remote add origin "$origin_url"
    /usr/bin/git push -q -u origin HEAD
  )
}

e2e_log "seeding fixture git upstream for all three projects (.boid/project.yaml + hooks committed)"
seed_project "$PROJ_A" "proj-a"
seed_project "$PROJ_B" "proj-b"
seed_project "$PROJ_C" "proj-c"

# --- pre-pull the sibling image host-side (same docker image store, DooD:
# the compose daemon and every job/sibling it creates all talk to this SAME
# host docker engine — build/container/compose.yml's own "Persistence"
# header comment) ------------------------------------------------------------
e2e_log "pre-pulling busybox:stable"
e2e_run docker pull busybox:stable

# --- diagnostic pre-flight: same uid + same bind mount the daemon service
# uses, but a plain `docker run` with no stdio-redirect games — isolates
# whether BOID_RUNTIME_DIR is actually writable by BOID_UID:BOID_GID
# inside a container BEFORE trying the full compose daemon (whose own
# stdout/stderr get redirected to boid.log as literally its first startup
# action, per compose.yml's own header comment, so a permission failure
# there produces zero visible docker-logs output — exactly the debugging
# dead end this pre-flight exists to avoid hitting blind).
e2e_log "pre-flight: verifying BOID_RUNTIME_DIR is writable as BOID_UID:BOID_GID inside a container"
docker run --rm \
  --user "${BOID_UID}:${BOID_GID}" \
  -v "${BOID_RUNTIME_DIR}:${BOID_RUNTIME_DIR}" \
  busybox:stable \
  sh -c "id; ls -ld '${BOID_RUNTIME_DIR}'; mkdir -p '${BOID_RUNTIME_DIR}/boid' && echo MKDIR_OK && touch '${BOID_RUNTIME_DIR}/boid/preflight.log' && echo TOUCH_OK && rm -f '${BOID_RUNTIME_DIR}/boid/preflight.log'"

# --- build image + bring up the compose stack -------------------------------
e2e_log "building image and starting compose stack (scripts/deploy-container.sh)"
e2e_run bash "$REPO_ROOT/scripts/deploy-container.sh"

DAEMON_SOCKET="$XDG_RUNTIME_DIR/boid.sock"
e2e_log "waiting for compose daemon health at $DAEMON_SOCKET"
e2e_run "$BUILD_DIR/boid-e2e" wait-health --timeout 30s --interval 200ms "$DAEMON_SOCKET"

# --- host-mode CLI verification (PR-3 Option 4 redesign, docs/plans/
# volume-only-daemon.md §論点c) ----------------------------------------------
# The daemon this script already brought up (via scripts/deploy-container.sh
# above, with BOID_CLI_TOKEN in its env — see this script's own "BOID_CLI_
# TOKEN" section) is the SAME daemon a real BOID_MODE=container CLI
# invocation would talk to; this section proves the dedicated CLI listener
# (internal/server.Config.CLIAddr/CLIToken) and cmd/host.go's own client
# path both work end to end, without a second, redundant compose deploy —
# ensureHostModeDaemon's own health probe finds the daemon already up and
# skips straight to dispatch.
CLI_ADDR="127.0.0.1:8442"
e2e_log "verifying the dedicated CLI listener at $CLI_ADDR"

# `|| true` on every curl call below (mirrors this script's own
# probe_http() helper further up): under `set -euo pipefail`, a bare
# `var="$(curl ...)"` with no guard would abort the WHOLE script on a
# connection-refused/timeout with an unhelpful shell error instead of the
# descriptive e2e_fail message immediately below each one. curl itself
# (no -f/--fail) already returns exit 0 for any HTTP response including
# 401/404 — only a transport-level failure needs guarding here.
#
# The health check retries for up to 10s: the app-level listener binding
# inside the container (confirmed by the daemon's own "cli listener
# started" log line) and the HOST-side docker port-publish/NAT rule
# actually routing to it are two separate events — `docker compose ps`
# reporting "Up" (and even boid-e2e's own unix-socket wait-health above
# succeeding, a completely different listener) does not guarantee the
# LATTER has caught up yet. A single one-shot curl right after wait-health
# hit exactly this race in practice (connection refused / curl code 000).
# The wrong/right-token checks immediately below do not need their own
# retry: by the time the health check above succeeds, the port-publish
# path is confirmed live for every subsequent request.
health_code=""
for _ in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; do
  health_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://${CLI_ADDR}/api/health" || true)"
  [[ "$health_code" == "200" ]] && break
  sleep 0.5
done
[[ "$health_code" == "200" ]] || e2e_fail "GET http://${CLI_ADDR}/api/health = ${health_code:-<no response>}, want 200 (public, no token) after retrying for 10s"

wrong_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H "Authorization: Bearer wrong-token" "http://${CLI_ADDR}/api/tasks" || true)"
[[ "$wrong_code" == "401" ]] || e2e_fail "GET http://${CLI_ADDR}/api/tasks with a wrong Bearer token = ${wrong_code:-<no response>}, want 401"

right_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H "Authorization: Bearer ${CLI_TOKEN}" "http://${CLI_ADDR}/api/tasks" || true)"
[[ "$right_code" == "200" ]] || e2e_fail "GET http://${CLI_ADDR}/api/tasks with the correct Bearer token = ${right_code:-<no response>}, want 200"
e2e_log "CLI listener token auth OK (health public, wrong token 401, correct token 200)"

e2e_log "verifying BOID_MODE=container CLI dispatch (cmd/host.go)"
host_mode_out="$(BOID_MODE=container BOID_COMPOSE_ROOT="$REPO_ROOT" "$BUILD_DIR/boid" task list -o json 2>&1)" || \
  e2e_fail "BOID_MODE=container 'boid task list' failed: $host_mode_out"
e2e_log "host-mode CLI dispatch OK"

# install_id (PR-2b): read from INSIDE the running daemon container, not a
# host-visible file — BOID_DATA_DIR is the boid_state named volume post-
# pivot, so ~/.local/share/boid/install_id only exists inside the
# container's own filesystem view. `docker compose exec` (not `run`: the
# daemon is already up) reads it back out over the SAME uid/volume the
# daemon itself uses.
INSTALL_ID="$(cd "$REPO_ROOT" && docker compose -f build/container/compose.yml exec -T daemon sh -c 'cat "$XDG_DATA_HOME/boid/install_id"' 2>/dev/null | tr -d '\r\n' || true)"
[[ -n "$INSTALL_ID" ]] || e2e_fail "could not read install_id from inside the daemon container (\$XDG_DATA_HOME/boid/install_id)"
e2e_log "install_id=$INSTALL_ID"

e2e_log "creating workspaces (ws-a/ws-b: capabilities.docker; ws-c: none)"
WS_DOCKER_YAML="$ROOT/ws-docker-capability.yaml"
cat > "$WS_DOCKER_YAML" <<'YAML'
capabilities:
  docker: {}
YAML
e2e_run "$BUILD_DIR/boid" workspace create ws-a --from-file "$WS_DOCKER_YAML"
e2e_run "$BUILD_DIR/boid" workspace create ws-b --from-file "$WS_DOCKER_YAML"
e2e_run "$BUILD_DIR/boid" workspace create ws-c

e2e_log "registering projects from the git fixture upstream (docs/plans/volume-only-daemon.md §論点a)"
e2e_run "$BUILD_DIR/boid" project add "https://${UPSTREAM_HOST}/e2e-fixture/proj-a.git" --workspace=ws-a
e2e_run "$BUILD_DIR/boid" project add "https://${UPSTREAM_HOST}/e2e-fixture/proj-b.git" --workspace=ws-b
e2e_run "$BUILD_DIR/boid" project add "https://${UPSTREAM_HOST}/e2e-fixture/proj-c.git" --workspace=ws-c

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

# --- broker TCP wire completion (docs/plans/phase6-cutover-followups.md
# §⓪) — dispatched FIRST, before task A/B's own docker-sibling dance:
# this pins a DIFFERENT, INDEPENDENT gap (broker RPC reachability, workspace
# C, no capabilities.docker at all) that has no dependency on the sibling
# containers task A/B set up below, so it must not be blocked by (or block
# on) that separate machinery — see this block's own placement history in
# git log if task A/B's own requirement 2 ever regresses again: running
# this check first keeps it validated independently either way.
e2e_log "dispatching verify-broker-tls (workspace C)"
task_c_out="$(create_task container-e2e-ws-c "verify broker TLS RPC" verify)"
printf '%s\n' "$task_c_out"
task_c_id="$(printf '%s\n' "$task_c_out" | parse_task_id)"
[[ -n "$task_c_id" ]] || e2e_fail "failed to parse task C id from: $task_c_out"
e2e_run "$BUILD_DIR/boid" action send --task "$task_c_id" --type start
wait_for_done "$task_c_id" "$ROOT/task_c.json"

task_c_json="$(cat "$ROOT/task_c.json")"
e2e_assert_contains "$task_c_json" '"status":"done"'
e2e_assert_contains "$task_c_json" '"result":"pass"'
e2e_assert_contains "$task_c_json" '"broker_transport":"tls"'
e2e_log "broker TCP wire (job -> broker -> daemon RPC over TLS) OK"

# Belt-and-suspenders structural pin alongside the functional one above:
# the broker's own TLS listener must be logged as NOT loopback-only — a
# sibling job container reaching it at all (which task C's own success
# above already proves) is only possible because of this bind-address
# change (docs/plans/phase6-cutover-followups.md §⓪'s first scope item);
# checking the daemon's own log for the literal bound address it logged at
# Start (internal/server/server.go's `slog.Info("broker tls listener
# started", "addr", addr)`) confirms the LISTENER itself, not just "some
# route happened to work".
#
# `docker compose logs` (NOT a boid.log FILE under $XDG_RUNTIME_DIR/boid/)
# is the actual source here: this compose deploy sets BOID_LOG_STDOUT=1
# (compose.yml), so the daemon writes its slog output to its own
# stdout/stderr — captured by `docker compose logs` — and never creates a
# boid.log file on disk at all under this configuration (confirmed
# empirically: dump_diagnostics' own "$daemon_log does not exist" branch
# fired on every run while this same information was visible in "docker
# compose logs (daemon)" the whole time — an earlier version of this check
# read the file path instead and failed every run with a false negative,
# even once the broker RPC itself was already succeeding).
broker_tls_log_line="$(cd "$REPO_ROOT" && docker compose -f build/container/compose.yml logs --no-color daemon 2>&1 | grep 'broker tls listener started' | tail -1 || true)"
[[ -n "$broker_tls_log_line" ]] || e2e_fail "daemon log has no 'broker tls listener started' line — the broker's TCP(mTLS) listener never bound at all"
e2e_log "broker tls listener log line: ${broker_tls_log_line}"
if printf '%s' "$broker_tls_log_line" | grep -q '127\.0\.0\.1'; then
  e2e_fail "broker TLS listener bound loopback-only (127.0.0.1) even though sandbox.backend: container is selected — gatewayBindHost wiring regressed"
fi

# git-push (workspace C, codex round-1, PR834 Minor 3): a SECOND, independent
# task on the same project — see .boid/hooks/verify-git-push.sh's own header
# comment for what it pins (remote.origin.url rewrite + git commit/push both
# succeeding from inside the per-job clone). Placed here, alongside task C's
# own broker-tls check and before the sibling A/B dance, for the same reason
# that check is dispatched first: no dependency on (and nothing here should
# block) the docker-sibling machinery task A/B exercise below.
e2e_log "dispatching verify-git-push (workspace C)"
task_c2_out="$(create_task container-e2e-ws-c "verify git push" git-push)"
printf '%s\n' "$task_c2_out"
task_c2_id="$(printf '%s\n' "$task_c2_out" | parse_task_id)"
[[ -n "$task_c2_id" ]] || e2e_fail "failed to parse task C2 (git-push) id from: $task_c2_out"
e2e_run "$BUILD_DIR/boid" action send --task "$task_c2_id" --type start
wait_for_done "$task_c2_id" "$ROOT/task_c2.json"

task_c2_json="$(cat "$ROOT/task_c2.json")"
e2e_assert_contains "$task_c2_json" '"status":"done"'
e2e_assert_contains "$task_c2_json" '"result":"pass"'
e2e_log "per-job clone git push (job -> gateway -> fixture upstream) reported OK"

# Strongest available check: read the push back directly off the fixture
# upstream's bare repo on the HOST filesystem — boid-e2e upstream-serve runs
# as a plain host process here (this script's own "fixture git upstream"
# section above), not inside any container, so its bare repos are ordinary,
# directly-readable directories under $UPSTREAM_DIR. This is independent of
# the job's own self-reported "pass" and is the actual regression codex
# round-1 Blocker 1 would have caused: a job whose `git commit` fails
# outright (the alternates-dependency failure mode) never reaches `git push`
# at all, so this repo's HEAD would simply never gain the new commit.
upstream_c_log="$(git -C "$UPSTREAM_DIR/e2e-fixture/proj-c.git" log --oneline -1 2>&1)" || \
  e2e_fail "reading fixture upstream proj-c.git log failed: $upstream_c_log"
e2e_assert_contains "$upstream_c_log" "e2e-container git push probe ${task_c2_id}"
e2e_log "fixture upstream proj-c.git HEAD carries the job's own push: ${upstream_c_log}"

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
printf '[e2e-container][latency] task A dispatch-to-done: %sms (docker, real DooD full cycle + PR-2b per-job clone — see docs/plans/phase6-container-backend.md §PR9 podman comparison ~150-165ms)\n' "$dispatch_ms"

task_a_json="$(cat "$ROOT/task_a.json")"
e2e_assert_contains "$task_a_json" '"status":"done"'
e2e_assert_contains "$task_a_json" '"result":"pass"'
e2e_log "requirements 1+2 (sibling reachable / cross-workspace isolated) OK"

e2e_log "waiting for setup-sibling (workspace B) to finish"
wait_for_done "$task_b_id" "$ROOT/task_b.json"
e2e_assert_contains "$(cat "$ROOT/task_b.json")" '"status":"done"'

e2e_log "container e2e scenario complete — teardown + requirement 3 verification runs in cleanup"
