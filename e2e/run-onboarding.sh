#!/usr/bin/env bash
set -euo pipefail

# e2e/run-onboarding.sh
#
# docs/plans/release-onboarding.md PR9: validates the "checkout-less
# binary only" onboarding path — a `go install github.com/novshi-tech/boid@latest`
# user has no boid repo checkout on disk at all, only the installed CLI
# binary — actually works end to end: `boid start` resolving its own
# version identity to a real GHCR image ref
# (internal/version.DefaultContainerImage), PULLING that image for real
# over the network (not a locally-built one bind-mounted in, which is all
# e2e/run-container.sh ever exercises — see its own header comment: that
# script always drives deployFromCheckout, the "a real checkout was found"
# path, and never touches cmd/host.go's deployFromEmbeddedAssets fallback
# at all), bringing the compose stack up, and a real job dispatching to
# completion against the resulting daemon.
#
# What makes this "checkout-less": BOID_COMPOSE_ROOT is left unset and the
# built test CLI is invoked from an empty scratch directory that contains
# no scripts/deploy-container.sh of its own. cmd/host.go's findComposeRoot
# (codex round-10 review of PR5) only ever consults BOID_COMPOSE_ROOT — it
# no longer walks up from cwd looking for a checkout — so this reliably
# forces cmd/host.go's deployFromEmbeddedAssets / cmd/start.go's
# runComposeUp fallback branch: extract the go:embed'd
# scripts/deploy-container.sh + build/container/{compose.yml,Dockerfile}
# this binary carries, and run that extracted script WITHOUT --build
# (there is no source tree in an extracted asset directory to build from
# regardless) — exactly what a real go-install-only user's `boid start`
# does.
#
# GHCR image resolution: internal/version.DefaultContainerImage() only
# ever names a GHCR ref for a build whose version identity is an EXACT
# release tag (vMAJOR.MINOR.PATCH, internal/version.IsExactRelease) — an
# ordinary CI checkout build of this branch is not one (no ldflags, no
# release tag on HEAD), so this script builds a dedicated test CLI with
# `-ldflags -X .../internal/version.buildVersion=$SENTINEL_VERSION` to
# simulate that build shape. SENTINEL_VERSION is a fixed, deliberately
# out-of-band version (major version 9999 — real releases are at v0.0.13
# as of this PR and increment the patch component only) rather than the
# actual latest release tag, for two reasons: (1) it makes this script
# self-contained — it builds AND pushes its own throwaway image under this
# tag every run (see "build + push the sentinel image" below), so it does
# not depend on any real release's image having ever been published under
# its own vMAJOR.MINOR.PATCH ref (none has: PR3, the GHCR-publish PR, only
# started tagging pushes with a release ref after v0.0.13 was already cut —
# see docs/plans/release-onboarding.md's PR分割案 table), and (2) reusing
# a fixed sentinel tag means every run overwrites the same GHCR tag rather
# than accumulating a new one per run.
#
# GHCR VISIBILITY (docs/plans/release-onboarding.md 決定4/PR3's own
# "VISIBILITY CAVEAT" in .github/workflows/blackbox-e2e.yml): whether
# ghcr.io/novshi-tech/boid-runner is public yet is an org-owner-only,
# one-time manual step whose completion this script cannot verify. This
# script authenticates (docker login ghcr.io) with the SAME GITHUB_TOKEN
# .github/workflows/blackbox-e2e.yml's container-image job already uses to
# PUSH — reading a package in the same org this token belongs to works
# whether or not that package is public — so it validates the
# pull -> compose-up -> dispatch MECHANICS unconditionally, regardless of
# the visibility flip's status. It does NOT prove an anonymous,
# unauthenticated `docker pull` (what a genuinely fresh `go install` user's
# own docker daemon would attempt, with no GHCR credentials at all)
# succeeds — that half stays unverified until the visibility flip actually
# happens; see this PR's own description for the current status.
#
# Requires a real docker engine + compose v2 plugin (same host requirement
# as e2e/run-container.sh — see that script's own header comment) plus
# GHCR push access to this org's boid-runner package (GITHUB_TOKEN /
# GITHUB_ACTOR, or equivalents) for the sentinel-tag push below. CI-only,
# like e2e/run-container.sh — not expected to run from a bare dev host.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# shellcheck source=/dev/null
source "$SCRIPT_DIR/lib/common.sh"

e2e_require_cmd docker
e2e_require_cmd go
e2e_require_cmd curl
if ! docker compose version >/dev/null 2>&1; then
  e2e_fail "docker compose (v2 plugin) not found — required by the embedded scripts/deploy-container.sh"
fi

if [[ -z "${GITHUB_TOKEN:-}" ]]; then
  e2e_fail "GITHUB_TOKEN must be set: this script pushes a throwaway sentinel-tagged image to ghcr.io/novshi-tech/boid-runner so it can pull it back for real (see this script's own header comment); it is CI-only, like e2e/run-container.sh"
fi
if [[ -z "${GITHUB_ACTOR:-}" ]]; then
  e2e_fail "GITHUB_ACTOR must be set (paired with GITHUB_TOKEN for docker login ghcr.io)"
fi

# See this script's own header comment for why this is a fixed,
# out-of-band sentinel rather than the actual latest release tag.
SENTINEL_VERSION="v9999.0.0"
SENTINEL_IMAGE="ghcr.io/novshi-tech/boid-runner:${SENTINEL_VERSION}"

ROOT="$(mktemp -d "${TMPDIR:-/tmp}/boid-e2e-onboarding-XXXXXX")"
KEEP_TEMP="${KEEP_TEMP:-0}"
BUILD_DIR="$ROOT/build"
mkdir -p "$BUILD_DIR"

# host.docker.internal (mirrors e2e/run-container.sh's own fixture-upstream
# section — see that script's header comment for the full rationale): must
# resolve on both this runner's own shell (the fixture repo seed `git push`
# below) and the compose daemon's container (compose.yml's own
# extra_hosts: host.docker.internal:host-gateway entry).
if ! grep -q 'host.docker.internal' /etc/hosts 2>/dev/null; then
  echo "127.0.0.1 host.docker.internal" | sudo tee -a /etc/hosts >/dev/null
fi

UPSTREAM_PID=""
COMPOSE_ASSETS_ROOT=""
INSTALL_ID=""
# (COMPOSE_ASSETS_ROOT is reassigned, unconditionally, right after
# XDG_STATE_HOME is set below — declared empty here only so dump_diagnostics
# has a defined variable to check even if the script dies before that point.)

dump_diagnostics() {
  printf '[e2e-onboarding] ===== docker compose logs (daemon) =====\n' >&2
  if [[ -n "$COMPOSE_ASSETS_ROOT" ]]; then
    (docker compose -f "$COMPOSE_ASSETS_ROOT/build/container/compose.yml" logs --no-color daemon 2>&1 | tail -n 300) >&2 || true
    printf '[e2e-onboarding] ===== docker compose ps -a (this stack) =====\n' >&2
    (docker compose -f "$COMPOSE_ASSETS_ROOT/build/container/compose.yml" ps -a 2>&1) >&2 || true
  else
    printf '[e2e-onboarding] (compose assets root not resolved yet; nothing to show)\n' >&2
  fi

  if [[ -n "$INSTALL_ID" ]]; then
    printf '[e2e-onboarding] ===== docker ps -a (this install) =====\n' >&2
    docker ps -a --filter "label=boid.install_id=${INSTALL_ID}" >&2 2>&1 || true
  fi

  for f in "$ROOT"/task_*.json; do
    [[ -f "$f" ]] || continue
    printf '[e2e-onboarding] ===== %s =====\n' "$f" >&2
    cat "$f" >&2 || true
  done
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

  # `boid stop` (docs/plans/release-onboarding.md 決定2/PR5) — the same
  # findComposeRoot-or-embedded-assets shape `boid start` used, sharing
  # withHostModeLock, so it resolves the SAME extracted compose root/stack
  # this run brought up. --volumes equivalent: compose.yml's own `down`
  # (scripts/deploy-container.sh's --down branch) always includes named
  # volumes — see that script's own comment on why a persistent runner
  # reusing $XDG_STATE_HOME/$XDG_CONFIG_HOME across invocations would
  # otherwise collide with this run's leftover boid_state.
  e2e_log "boid stop (tearing down the compose stack)"
  "$BUILD_DIR/boid" stop >"$ROOT/stop.log" 2>&1 || \
    printf '[e2e-onboarding] WARN: boid stop failed, see %s\n' "$ROOT/stop.log" >&2
  cat "$ROOT/stop.log" >&2 || true

  if [[ -n "$INSTALL_ID" ]]; then
    e2e_log "boid reap --install-id ${INSTALL_ID} --include-workspace-homes"
    "$BUILD_DIR/boid" reap --install-id "$INSTALL_ID" --include-workspace-homes >"$ROOT/reap.log" 2>&1 || \
      printf '[e2e-onboarding] WARN: boid reap failed, see %s\n' "$ROOT/reap.log" >&2
    cat "$ROOT/reap.log" >&2 || true

    local leaked
    leaked="$(docker ps -aq --filter "label=boid.install_id=${INSTALL_ID}")$(docker network ls -q --filter "label=boid.install_id=${INSTALL_ID}")$(docker volume ls -q --filter "label=boid.install_id=${INSTALL_ID}")$(docker volume ls -q --filter "label=boid.workspace_home_install_id=${INSTALL_ID}")"
    if [[ -n "$leaked" ]]; then
      printf '[e2e-onboarding] ===== leaked resources after reap (install_id=%s) =====\n' "$INSTALL_ID" >&2
      docker ps -a --filter "label=boid.install_id=${INSTALL_ID}" >&2 || true
      docker network ls --filter "label=boid.install_id=${INSTALL_ID}" >&2 || true
      docker volume ls --filter "label=boid.install_id=${INSTALL_ID}" >&2 || true
      docker volume ls --filter "label=boid.workspace_home_install_id=${INSTALL_ID}" >&2 || true
      exit_code=1
    fi
  fi

  if [[ $exit_code -ne 0 || $KEEP_TEMP -eq 1 ]]; then
    printf '[e2e-onboarding] temp root preserved at %s\n' "$ROOT" >&2
  else
    rm -rf "$ROOT" >/dev/null 2>&1 || true
  fi
  exit "$exit_code"
}
trap cleanup EXIT

# --- build the boid + boid-e2e test binaries --------------------------------
# The main "boid" binary carries the -ldflags version-identity override (see
# this script's own header comment for SENTINEL_VERSION); boid-e2e (the
# fixture-upstream helper) needs no such override.
e2e_log "building the onboarding-e2e boid binary (buildVersion=${SENTINEL_VERSION})"
e2e_run go build \
  -ldflags "-X github.com/novshi-tech/boid/internal/version.buildVersion=${SENTINEL_VERSION}" \
  -o "$BUILD_DIR/boid" "$REPO_ROOT"
e2e_log "building boid-e2e helper"
e2e_run go build -o "$BUILD_DIR/boid-e2e" "$REPO_ROOT/e2e/cmd/boid-e2e"
PATH="$BUILD_DIR:$PATH"
export PATH

# --- throwaway XDG layout ----------------------------------------------------
# Isolates every stateful path deployFromEmbeddedAssets/loadOrCreateCLIToken/
# withHostModeLock touch (~/.config/boid, ~/.local/state/boid) from whatever
# this CI runner's own real HOME might otherwise carry, mirroring
# e2e/run-container.sh's own "throwaway XDG layout" section.
export XDG_DATA_HOME="$ROOT/data"
export XDG_CONFIG_HOME="$ROOT/config"
export XDG_STATE_HOME="$ROOT/state"
export XDG_RUNTIME_DIR="$ROOT/run"
mkdir -p "$XDG_DATA_HOME/boid" "$XDG_CONFIG_HOME/boid" "$XDG_STATE_HOME/boid" "$XDG_RUNTIME_DIR"

# Deterministic regardless of whether deployFromEmbeddedAssets ever
# succeeds (hostModeAssetsDir()'s own path shape, cmd/host.go) — computed
# here, unconditionally, so dump_diagnostics can find it even on a failure
# that happens mid-extraction/mid-deploy, not only after a full success.
COMPOSE_ASSETS_ROOT="$XDG_STATE_HOME/boid/compose"

# --- build + push the sentinel image -----------------------------------------
# This is the ONE place this script uses the real checkout (build/container/
# Dockerfile's `COPY . .` build context needs it) — everything from here on
# runs the boid binary itself from an empty, checkout-less scratch directory
# (see "checkout-less scratch dir" below), exactly mirroring the two-path
# split cmd/host.go's own header comment describes (deployFromCheckout can
# only ever run from a real checkout; deployFromEmbeddedAssets is the only
# path this script's later steps ever exercise).
#
# BOID_VERSION is baked in via the same build-arg
# .github/workflows/blackbox-e2e.yml's own container-image job uses for a
# real release build, set to SENTINEL_VERSION so the DAEMON process running
# inside this image also reports itself as that exact "release" — which
# matters for job containers too: containerBackend's own default job image
# (internal/dispatcher/container_backend.go:553) is
# version.DefaultContainerImage() evaluated INSIDE the running daemon, so a
# daemon that does not believe it is SENTINEL_VERSION would resolve a
# DIFFERENT (nonexistent) image ref for every job container it launches.
e2e_log "docker login ghcr.io as ${GITHUB_ACTOR}"
echo "$GITHUB_TOKEN" | docker login ghcr.io -u "$GITHUB_ACTOR" --password-stdin

e2e_log "building the sentinel boid-runner image (BOID_VERSION=${SENTINEL_VERSION})"
LOCAL_SENTINEL_TAG="boid-runner:onboarding-e2e-sentinel"
e2e_run docker build \
  --build-arg "BOID_VERSION=${SENTINEL_VERSION}" \
  -t "$LOCAL_SENTINEL_TAG" \
  -f "$REPO_ROOT/build/container/Dockerfile" \
  "$REPO_ROOT"
docker tag "$LOCAL_SENTINEL_TAG" "$SENTINEL_IMAGE"

e2e_log "pushing ${SENTINEL_IMAGE} to GHCR"
e2e_run docker push "$SENTINEL_IMAGE"

# Remove every local tag pointing at this image BEFORE the later `boid
# start` — otherwise `docker compose up`'s default pull_policy ("missing":
# pull only if absent locally) would silently satisfy the pull from this
# process's own local build instead of actually reaching GHCR over the
# network, which is the entire thing this script exists to verify.
e2e_log "removing local image tags so the later pull is a real network pull"
docker rmi "$LOCAL_SENTINEL_TAG" "$SENTINEL_IMAGE" >/dev/null 2>&1 || true

# --- fixture git upstream (mirrors e2e/run-container.sh's own "fixture git
# upstream" section — see that script's header comment for why 0.0.0.0 +
# host.docker.internal, not the shared e2e_setup_fixture_upstream helper's
# 127.0.0.1 default). Started BEFORE `boid start` below, deliberately: the
# daemon container only ever reads SSL_CERT_FILE/GIT_SSL_CAINFO once, at
# container CREATE time (build/container/compose.yml's own passthrough env
# entries), so both must already be exported in THIS shell's environment
# before scripts/deploy-container.sh's `compose up` runs — starting the
# fixture upstream (and exporting these) any later leaves the already-
# running daemon container with no trust anchor for the fixture's
# self-signed cert, and its own `git clone` (project registration below;
# PR-2b, the daemon clones a project's git URL itself) fails with "server
# certificate verification failed" (exactly what an earlier revision of
# this script hit — see git history / this PR's own review notes). -------
UPSTREAM_DIR="$ROOT/upstream-repos"
UPSTREAM_READY="$ROOT/upstream.addr"
UPSTREAM_CERT="$XDG_RUNTIME_DIR/upstream-ca.crt"
mkdir -p "$UPSTREAM_DIR"

e2e_log "starting fixture git upstream (0.0.0.0, host.docker.internal-reachable)"
"$BUILD_DIR/boid-e2e" upstream-serve \
  --dir "$UPSTREAM_DIR" \
  --addr "0.0.0.0:0" \
  --ready-file "$UPSTREAM_READY" \
  --cert-file "$UPSTREAM_CERT" \
  "e2e-fixture/proj-onboarding" \
  >"$ROOT/upstream.stdout.log" 2>"$ROOT/upstream.stderr.log" &
UPSTREAM_PID=$!

e2e_wait_for_file "$UPSTREAM_READY" 10
UPSTREAM_BOUND="$(cat "$UPSTREAM_READY")"
UPSTREAM_PORT="${UPSTREAM_BOUND##*:}"
UPSTREAM_HOST="host.docker.internal:${UPSTREAM_PORT}"
e2e_log "fixture upstream bound on ${UPSTREAM_BOUND}, reachable via ${UPSTREAM_HOST}"

# Exported before `boid start` (below) — see this block's own header
# comment. UPSTREAM_CERT lives under $XDG_RUNTIME_DIR (-> BOID_RUNTIME_DIR),
# the one host<->container bind that IS load-bearing here (see this
# script's own "throwaway XDG layout" section), so the path is valid
# inside the daemon container too, not just on this runner's own
# filesystem.
export SSL_CERT_FILE="$UPSTREAM_CERT"
export GIT_SSL_CAINFO="$UPSTREAM_CERT"

# --- checkout-less scratch dir -----------------------------------------------
# The defining condition of "checkout-less": BOID_COMPOSE_ROOT unset (never
# exported anywhere in this script) AND a cwd with no scripts/
# deploy-container.sh of its own for findComposeRoot to find — though as of
# codex round-10 review of PR5, findComposeRoot no longer even looks at cwd
# at all, only BOID_COMPOSE_ROOT, so the directory choice here is defense in
# depth / realism (a real go-install user's cwd is an ordinary project
# directory, not a boid checkout) rather than strictly load-bearing.
SCRATCH_DIR="$ROOT/scratch-no-checkout"
mkdir -p "$SCRATCH_DIR"

# --- boid start (embedded-assets fallback: pull -> compose up -> health) ----
# Deliberately does NOT set BOID_COMPOSE_ROOT. runStart (cmd/start.go) calls
# runComposeUp, which — finding no BOID_COMPOSE_ROOT — falls through to
# deployFromEmbeddedAssets (cmd/host.go): extracts this binary's own
# go:embed'd scripts/deploy-container.sh + build/container/{compose.yml,
# Dockerfile} into $XDG_STATE_HOME/boid/compose, sets
# BOID_IMAGE=version.DefaultContainerImage() (== SENTINEL_IMAGE for this
# binary), and runs that extracted script WITHOUT --build — its own
# pull-first default (docs/plans/release-onboarding.md 穴4/PR4) pulls
# SENTINEL_IMAGE from GHCR for real (the local copy was removed above).
# `boid start` blocks internally on waitForHealthy (up to
# hostModeStartTimeout, 5 minutes) before returning, so a zero exit here
# already proves pull -> compose up -> health succeeded end to end.
e2e_log "boid start (checkout-less: BOID_COMPOSE_ROOT unset, cwd=$SCRATCH_DIR, expect a real GHCR pull of ${SENTINEL_IMAGE})"
(cd "$SCRATCH_DIR" && e2e_run "$BUILD_DIR/boid" start)

if [[ ! -f "$COMPOSE_ASSETS_ROOT/build/container/compose.yml" ]]; then
  e2e_fail "expected extractComposeAssets to have written $COMPOSE_ASSETS_ROOT/build/container/compose.yml (embedded-assets fallback did not run as expected)"
fi
e2e_log "confirmed the embedded-assets fallback ran: $COMPOSE_ASSETS_ROOT/build/container/compose.yml exists"

# --- confirm the daemon is genuinely running the just-pulled sentinel
# image, not some stale/local one ----------------------------------------
daemon_cid="$(docker compose -f "$COMPOSE_ASSETS_ROOT/build/container/compose.yml" ps -q daemon)"
[[ -n "$daemon_cid" ]] || e2e_fail "docker compose ps -q daemon returned no container id"
running_image="$(docker inspect "$daemon_cid" --format '{{.Config.Image}}' 2>/dev/null || true)"
e2e_log "daemon service is running image: ${running_image:-<unknown>}"
[[ "$running_image" == "$SENTINEL_IMAGE" ]] || e2e_fail "daemon container is running '${running_image:-<unknown>}', expected the just-pulled sentinel image '${SENTINEL_IMAGE}'"

# --- CLI listener sanity check (mirrors e2e/run-container.sh's own "host-
# mode CLI verification" section, trimmed to the health probe only — the
# full auth-matrix check already lives there and does not need duplicating
# here) ------------------------------------------------------------------
CLI_ADDR="127.0.0.1:8442"
health_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://${CLI_ADDR}/api/health" || true)"
[[ "$health_code" == "200" ]] || e2e_fail "GET http://${CLI_ADDR}/api/health = ${health_code:-<no response>}, want 200"
e2e_log "CLI listener health OK"

CLI_TOKEN="$(cat "$XDG_CONFIG_HOME/boid/cli-token")"

INSTALL_ID="$(docker compose -f "$COMPOSE_ASSETS_ROOT/build/container/compose.yml" exec -T daemon sh -c 'cat "$XDG_DATA_HOME/boid/install_id"' 2>/dev/null | tr -d '\r\n' || true)"
[[ -n "$INSTALL_ID" ]] || e2e_fail "could not read install_id from inside the daemon container"
e2e_log "install_id=$INSTALL_ID"

# --- fixture project: a single trivial hook, no docker capability needed —
# this script's job is proving the checkout-less bootstrap + dispatch
# mechanics, not re-covering e2e/run-container.sh's own sibling-
# connectivity/workspace-isolation requirements. -----------------------
PROJ="$ROOT/fixture-src/proj-onboarding"
mkdir -p "$PROJ/.boid/hooks"
cat > "$PROJ/.boid/project.yaml" <<'YAML'
id: onboarding-e2e-proj
name: Onboarding E2E project
task_behaviors:
  verify:
    hooks:
      - id: verify-onboarding
        command: |
          printf '{"artifact":{"result":"pass"}}\n' | boid task update --payload-patch @-
    name: verify
YAML

(
  cd "$PROJ"
  /usr/bin/git init -q -b main
  /usr/bin/git config user.name "E2E Onboarding Fixture"
  /usr/bin/git config user.email "e2e-onboarding-fixture@boid.test"
  /usr/bin/git add -A
  /usr/bin/git commit -q -m "e2e-onboarding fixture seed"
  /usr/bin/git remote add origin "https://${UPSTREAM_HOST}/e2e-fixture/proj-onboarding.git"
  /usr/bin/git push -q -u origin HEAD
)

# --- register + dispatch: the "boid project add ... --workspace=default"
# step from docs/plans/release-onboarding.md's own 目標オンボーディングフロー.
# "default" is the reserved implicit workspace slug
# (internal/orchestrator/workspace_slug.go's DefaultWorkspaceSlug) — no
# separate `boid workspace create` needed. -------------------------------
e2e_log "registering the fixture project (git URL, --workspace=default)"
e2e_run "$BUILD_DIR/boid" project add "https://${UPSTREAM_HOST}/e2e-fixture/proj-onboarding.git" --workspace=default

task_out="$("$BUILD_DIR/boid" task create <<YAML
project_id: onboarding-e2e-proj
title: verify onboarding dispatch
behavior: verify
YAML
)"
printf '%s\n' "$task_out"
task_id="$(printf '%s\n' "$task_out" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id from: $task_out"

e2e_log "dispatching task $task_id"
e2e_run "$BUILD_DIR/boid" action send --task "$task_id" --type start

deadline=$((SECONDS + 120))
task_json_file="$ROOT/task_a.json"
while true; do
  "$BUILD_DIR/boid-e2e" get-task "$task_id" >"$task_json_file" 2>/dev/null || true
  if grep -q '"status":"done"' "$task_json_file" 2>/dev/null; then
    break
  fi
  if grep -q '"status":"aborted"' "$task_json_file" 2>/dev/null; then
    e2e_fail "task $task_id aborted: $(cat "$task_json_file")"
  fi
  if [[ $SECONDS -ge $deadline ]]; then
    e2e_fail "timeout waiting for task $task_id to finish: $(cat "$task_json_file")"
  fi
  sleep 0.5
done

task_json="$(cat "$task_json_file")"
e2e_assert_contains "$task_json" '"status":"done"'
e2e_assert_contains "$task_json" '"result":"pass"'
e2e_log "checkout-less onboarding e2e complete: pull -> compose up -> dispatch all succeeded"

# Belt-and-suspenders: `right_code` auth check, matching
# e2e/run-container.sh's own CLI listener verification, confirms the CLI
# token this run's own boid start/project add/task create/action send calls
# authenticated with is the one actually enforced (not merely present).
right_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -H "Authorization: Bearer ${CLI_TOKEN}" "http://${CLI_ADDR}/api/tasks" || true)"
[[ "$right_code" == "200" ]] || e2e_fail "GET http://${CLI_ADDR}/api/tasks with the CLI token = ${right_code:-<no response>}, want 200"

e2e_log "teardown + reap verification runs in cleanup"
