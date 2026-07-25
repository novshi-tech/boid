#!/usr/bin/env bash
#
# scripts/deploy-container.sh
#
# Deploy script for the container-backend daemon stack
# (docs/plans/phase6-container-backend.md §PR6 / §PR7). Builds the shared
# base image (build/container/Dockerfile) and (re)starts the
# build/container/compose.yml daemon stack against it.
#
# The PR6-era "DO NOT RUN THIS AGAINST A REAL ~/.local/share/boid YET"
# warning that previously lived here has been retired: PR7 (#823) landed the
# three prerequisites the warning was gating on —
#   1. startup reap of orphan sibling job containers
#      (SweepOrphans + install_id-scoped ReapOrphans, wired through
#      MarkStale*↔auto-reopen so reap-failed tasks are skipped for reopen)
#   2. Wait single-ownership guarantee
#      (wire fail-hard on global reap error to prevent double-execution)
#   3. persistent transcript spool
#      (container_backend transcript disk spool with sync-before-close and
#      fail-hard on spool disk failures)
# Plus §⓪ (broker TCP wire, #825) and §⓪-b (egress proxy dotless refuse,
# #826) landed 2026-07-23. Running this script for real against those
# prerequisites is what surfaced the volume-only pivot in the first place
# (docs/plans/volume-only-daemon.md — a fatal auto-prune/DB-loss incident
# caused by the compose daemon's own host filesystem visibility not
# matching what the host process registering projects saw). This script
# now deploys the volume-only redesign build/container/compose.yml
# implements: daemon data/config live in named volumes generated on first
# boot, not host-bind-mounted directories this script used to have to
# pre-provision (see this script's own diff history / PR-1a of the
# volume-only-daemon.md rollout for what changed and why).
#
# host has no docker engine, only podman (CLAUDE.md as of 2026-07-24) — the
# `docker`-branch below is exactly what CI
# (docs/plans/phase6-container-backend.md §PR9's e2e-container job, on a
# real-docker ubuntu-24.04 runner) exercises; the `podman`/`podman-compose`
# fallback is what a podman-only dev host (nose's, per volume-only-daemon.md
# §論点 i's 案 X decision — podman promoted to a supported primary target,
# not just a best-effort fallback) actually runs day to day.

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCKERFILE="$ROOT_DIR/build/container/Dockerfile"
COMPOSE_FILE="$ROOT_DIR/build/container/compose.yml"

# --- select an engine -------------------------------------------------------
# Prefers docker (compose v2 syntax, DOCKER_HOST semantics dockerproxy/
# containerBackend are written against, and what CI's e2e-container job
# uses) but treats podman as a fully supported second engine, not a
# best-effort fallback (docs/plans/volume-only-daemon.md §論点 i's 案 X:
# "podman のほうがセキュリティに優れる、podman で動かせることは必須").
#
# usable() checks more than PATH presence (round-2 codex review Major 4): a
# host can have a `docker` CLI installed with no reachable daemon/socket
# (stale install, permission issue, daemon not running) while a genuinely
# working `podman` sits right next to it — presence-only selection would
# pick the unusable docker every time and never fall back. `docker
# version`/`podman version` round-trips to the actual engine, not just the
# CLI binary.
usable() {
	command -v "$1" >/dev/null 2>&1 && "$1" version >/dev/null 2>&1
}

if usable docker; then
	ENGINE=docker
	BUILD_CMD=(docker build)
	COMPOSE_CMD=(docker compose -f "$COMPOSE_FILE")
elif usable podman; then
	ENGINE=podman
	BUILD_CMD=(podman build)
	if command -v podman-compose >/dev/null 2>&1; then
		COMPOSE_CMD=(podman-compose -f "$COMPOSE_FILE")
	else
		COMPOSE_CMD=()
		echo "warning: podman found but no podman-compose; skipping the compose up/down step (image build only)" >&2
	fi
elif command -v docker >/dev/null 2>&1; then
	echo "error: docker is on PATH but not usable ('docker version' failed — daemon not running/unreachable), and no usable podman was found either" >&2
	exit 1
elif command -v podman >/dev/null 2>&1; then
	echo "error: podman is on PATH but not usable ('podman version' failed), and no usable docker was found either" >&2
	exit 1
else
	echo "error: neither docker nor podman found on PATH" >&2
	exit 1
fi
echo "deploy-container: using engine=$ENGINE"

# --- podman: engine socket + pre-flight (docs/plans/volume-only-daemon.md
# §論点 i, 案 X) ---------------------------------------------------------
# Docker's DooD sibling-container path (build/container/compose.yml's own
# "Topology" header comment) bind-mounts an engine socket into the daemon
# container. Docker's own socket is always /var/run/docker.sock — no
# per-host variance — but podman rootless's equivalent lives under the
# invoking user's XDG runtime dir instead, and is not guaranteed to be
# listening at all (podman.socket is an opt-in systemd user unit, not
# started by a bare `podman` install). Both are handled here rather than
# left to compose.yml's own `${BOID_DOCKER_SOCK_SRC:-/var/run/docker.sock}`
# default, which is deliberately docker-shaped (see that variable's own
# doc comment in compose.yml) — this script's whole job is to compute the
# right override for whichever engine it actually found.
if [[ "$ENGINE" == "podman" ]]; then
	: "${BOID_DOCKER_SOCK_SRC:=/run/user/$(id -u)/podman/podman.sock}"
	export BOID_DOCKER_SOCK_SRC
	echo "deploy-container: podman engine — BOID_DOCKER_SOCK_SRC=$BOID_DOCKER_SOCK_SRC"

	# A missing/inactive podman.socket produces a confusing failure much
	# later (compose up's bind mount silently succeeds against a
	# not-yet-existing path under some engine/compose version
	# combinations, then every docker-API call the daemon makes once
	# running fails with a bare connection-refused/no-such-file — no hint
	# that the fix is a systemd unit) — checked explicitly here instead,
	# with the actual remediation printed.
	if ! systemctl --user is-active podman.socket >/dev/null 2>&1; then
		echo "error: podman.socket is not active — required for the DooD engine-socket bind (BOID_DOCKER_SOCK_SRC=$BOID_DOCKER_SOCK_SRC)." >&2
		echo "  fix: systemctl --user enable --now podman.socket" >&2
		exit 1
	fi
fi

# --- compute the required compose env vars -----------------------------
# BOID_RUNTIME_DIR mirrors internal/client.DefaultSocketPath()'s exact
# fallback chain, not just its XDG_RUNTIME_DIR-or-/run/user/<uid> shape
# (Major 12, PR6 codex review): DefaultSocketPath only uses
# /run/user/<uid> when os.Stat confirms that directory actually exists on
# THIS host — it is not systemd-logind-managed on every host (a headless
# server with no active login session, some minimal container base
# images, ...) — falling back to a bare /tmp/boid-<uid>.sock file
# otherwise. Diverging from what a bare `boid start` on the SAME host
# resolves to would break §決定4's "server socket の host 同一 path bind
# (相互排他)" contract in exactly the case it matters most (this stack's
# whole reason to run resolving a DIFFERENT socket path than the host
# daemon it's meant to coexist with / roll back to, so both start
# "successfully" as two live daemons at once — still a live concern until
# a later PR retires the CLI's reliance on this bind mount, docs/plans/
# volume-only-daemon.md §論点 c). DefaultSocketPath's own BOID_SOCKET
# override (an arbitrary full path, not a directory) has no
# bind-mountable-directory equivalent here and is intentionally not
# replicated — an operator using it must set BOID_RUNTIME_DIR (or
# BOID_SOCKET's own containing directory) manually.
#
# BOID_DATA_DIR/BOID_CONFIG_DIR/XDG_DATA_HOME/XDG_CONFIG_HOME are gone
# (docs/plans/volume-only-daemon.md §論点 d): those directories are now
# daemon-owned named volumes mounted at compose.yml's own fixed,
# hardcoded container paths, not host paths this script needs to compute
# or pre-provision.
if [[ -z "${BOID_RUNTIME_DIR:-}" ]]; then
	if [[ -n "${XDG_RUNTIME_DIR:-}" ]]; then
		BOID_RUNTIME_DIR="$XDG_RUNTIME_DIR"
	elif [[ -d "/run/user/$(id -u)" ]]; then
		BOID_RUNTIME_DIR="/run/user/$(id -u)"
	else
		# Mirrors DefaultSocketPath()'s /tmp/boid-<uid>.sock fallback: the
		# containing directory is plain /tmp, not a boid-owned
		# subdirectory of it.
		BOID_RUNTIME_DIR="/tmp"
	fi
fi
: "${BOID_UID:=$(id -u)}"
: "${BOID_GID:=$(id -g)}"
# DOCKER_GID (Major 9, PR6 codex review): the host's `docker` group GID,
# so compose.yml's group_add can grant the non-root daemon process
# permission to open the engine socket (DooD). `getent group docker` is
# the portable way to look this up (works whether the group entry comes
# from /etc/group or an NSS backend); if the host has no `docker` group at
# all (e.g. a podman-only host with no docker-shaped group, CLAUDE.md's
# noted dev-host state), fall back to compose.yml's own
# `${DOCKER_GID:-999}" default rather than failing here — group_add with
# a GID that doesn't exist on this host is harmless (docker/podman does
# not validate it against /etc/group), and 999 is podman-compose 1.0.6's
# own requirement (an unset var used in a list context fails
# interpolation on some versions) as well as a common `docker.io`
# package default.
: "${DOCKER_GID:=$(getent group docker 2>/dev/null | cut -d: -f3)}"
: "${DOCKER_GID:=999}"
export BOID_RUNTIME_DIR BOID_UID BOID_GID DOCKER_GID

IMAGE_TAG="boid:$(git -C "$ROOT_DIR" rev-parse HEAD)"

echo "deploy-container: building $IMAGE_TAG from $DOCKERFILE"
"${BUILD_CMD[@]}" \
	--build-arg "BOID_UID=$BOID_UID" \
	--build-arg "BOID_GID=$BOID_GID" \
	-t "$IMAGE_TAG" \
	-t boid-runner:latest \
	-f "$DOCKERFILE" \
	"$ROOT_DIR"

# DEPLOY_CONTAINER_BUILD_ONLY (codex round-1, PR834 Blocker 2): lets a
# caller (e2e/run-container.sh) invoke just this build step, standalone,
# BEFORE it needs the resulting boid-runner:latest image for its own
# `docker compose run` config-seed step — compose.yml's `daemon` service has
# no `build:` section (it only ever references the image by tag), so any
# `docker compose run`/`up` against a fresh runner with no local
# boid-runner:latest image would otherwise try to PULL a nonexistent/private
# image instead of building one. This script's own later, unconditional
# build (this exact step, re-run) stays cheap on a second invocation thanks
# to docker/podman's own layer cache.
if [[ "${DEPLOY_CONTAINER_BUILD_ONLY:-0}" == "1" ]]; then
	echo "deploy-container: DEPLOY_CONTAINER_BUILD_ONLY=1 — image built ($IMAGE_TAG); skipping compose seed/up (caller owns those steps)"
	exit 0
fi

if [[ ${#COMPOSE_CMD[@]} -eq 0 ]]; then
	# round-2 codex review Major 4: this used to print a warning and exit 0
	# here — reporting overall success even though the compose stack was
	# NEVER started, which left a caller polling the daemon's health
	# endpoint for the full timeout with no way to tell "still starting"
	# apart from "will never start". DEPLOY_CONTAINER_BUILD_ONLY=1 (handled
	# above, before this point) remains the one legitimate "image-only,
	# caller owns the rest" exit — reaching here means the caller wanted an
	# actually-running stack and podman-compose is simply missing, which is
	# an unmet requirement, not a degraded-but-OK outcome.
	echo "error: podman-compose is required to bring the compose stack up (image built: $IMAGE_TAG, but compose up was skipped — see warning above); install podman-compose or set DEPLOY_CONTAINER_BUILD_ONLY=1 if you only wanted the image" >&2
	exit 1
fi

# --- pre-provision the still-bind-mounted runtime dir -----------------------
# BOID_RUNTIME_DIR must exist and be owned by BOID_UID:BOID_GID BEFORE
# `compose up` (Major 13, PR6 codex review, narrowed to just this one
# directory now that BOID_DATA_DIR/BOID_CONFIG_DIR are named volumes the
# daemon itself owns and no longer need host pre-provisioning): compose/
# docker/podman auto-create a missing bind-mount host path, but as root
# (or whichever uid runs the engine daemon) — the non-root daemon process
# (user: ${BOID_UID}:${BOID_GID} in compose.yml) would then be unable to
# even see a live socket under BOID_RUNTIME_DIR on a genuinely first-ever
# run against a fresh layout. chown is best-effort (a warning, not
# fatal): it fails harmlessly when this script is not running as the
# target uid/gid and lacks permission to chown to it (e.g. BOID_UID
# overridden to something other than the invoking user) — in that case
# the directory most likely already has the right ownership (created
# by/for that uid outside this script) and this is a no-op.
echo "deploy-container: ensuring BOID_RUNTIME_DIR ($BOID_RUNTIME_DIR) exists and is owned by ${BOID_UID}:${BOID_GID}"
mkdir -p "$BOID_RUNTIME_DIR"
chown "$BOID_UID:$BOID_GID" "$BOID_RUNTIME_DIR" 2>/dev/null || \
	echo "warning: could not chown $BOID_RUNTIME_DIR to ${BOID_UID}:${BOID_GID} (continuing — it may already be owned correctly)" >&2

# --- seed config.yaml (first boot only) --------------------------------
# codex round-1, PR834 Major 2: this used to run AFTER `compose up -d`
# below (as a printed instruction telling the operator to have already done
# it, which is unusable — the daemon has by then already started against
# whatever config.yaml happens to pre-exist in the volume, i.e. none, i.e.
# the userns-backend default, on a genuinely first deploy). `boid config`
# still has no bootstrap-before-first-boot path of its own (docs/plans/
# volume-only-daemon.md §論点f) — until it does, this script seeds
# sandbox.backend directly, the same way e2e/run-container.sh's own "seed
# config.yaml" step does: `docker compose run --rm --entrypoint sh daemon`
# is a ONE-OFF container from the exact same service definition (same
# image, same boid_state volume mount, same BOID_UID:BOID_GID `user:`)
# purely to write the file, before the real long-running daemon (`up`
# below) ever starts. Idempotent: only writes config.yaml if the volume
# doesn't already have one, so re-running this script against a live,
# already-configured deploy never clobbers an operator's own edits (e.g. a
# later `boid config set` against the running daemon, or a hand-edited
# config.yaml with settings beyond just sandbox.backend).
echo "deploy-container: seeding config.yaml into the boid_state volume (sandbox.backend: container) if not already present"
"${COMPOSE_CMD[@]}" run --rm -T --entrypoint sh daemon -c '
set -e
mkdir -p "$XDG_CONFIG_HOME/boid"
if [ -f "$XDG_CONFIG_HOME/boid/config.yaml" ]; then
	echo "deploy-container: config.yaml already exists in the boid_state volume; leaving it as-is" >&2
else
	cat > "$XDG_CONFIG_HOME/boid/config.yaml" <<YAML
sandbox:
  backend: container
YAML
	echo "deploy-container: seeded default config.yaml (sandbox.backend: container) into the boid_state volume" >&2
fi
'

# --- validate the effective backend (codex round-3, PR834 Major 2) ------
# The seed step above is idempotent: an EXISTING config.yaml in the volume
# is left untouched, on the reasonable assumption that preserving an
# operators own edits is the right default. But an existing config.yaml
# left over from a prior userns-backend install (or one that only ever set
# unrelated keys, e.g. web:, with no sandbox: block at all — which resolves
# to the userns default) means the daemon this script is about to start
# would silently run the userns backend while this script still prints
# "sandbox.backend: container, seeded above" below, as if the container
# backend were actually running.
#
# Round-2 parsed this with a sed one-liner scanning the sandbox: block for
# any indented `backend:` line — codex round-3 flagged that as unsafe: it
# accepts a `backend:` key nested ARBITRARILY deep under sandbox: (e.g.
# `sandbox:\n  ignored:\n    backend: container`), which Go's yaml.v3
# decoder (Config.UnmarshalYAML) leaves unset — the real daemon would
# select userns while the sed scan reported container — and it
# false-rejects a quoted `"container"` or a folded scalar value, neither of
# which sed's plain-text match handles. `boid config get` (cmd/config.go)
# still can't help here — it always talks to a LIVE daemon's HTTP API, which
# does not exist yet at this point in a fresh deploy (the daemon has not
# started) or a one-off seed/validate container (which never runs `boid
# start`, only this shell) — but `internal/config.LoadFromPath` (the
# primitive `boid config get` itself sits on top of) is a pure
# filesystem-read + yaml.v3 decode with NO daemon dependency at all, so the
# NEW `boid config effective-backend <path>` subcommand (cmd/config.go,
# scopeLocal — see its own doc comment) exposes exactly that: the same
# nesting/quoting/folded-scalar semantics — and the same hard-error-on-
# unrecognized-value behavior — Config.UnmarshalYAML applies at real daemon
# startup, run standalone against an explicit path via the boid binary
# already baked into this image (build/container/Dockerfile's
# `/usr/local/bin/boid`).
echo "deploy-container: verifying the boid_state volume config.yaml resolves sandbox.backend to container"
"${COMPOSE_CMD[@]}" run --rm -T --entrypoint sh daemon -c '
set -e
cfg="$XDG_CONFIG_HOME/boid/config.yaml"
if ! backend=$(/usr/local/bin/boid config effective-backend "$cfg"); then
	echo "deploy-container: ERROR: the boid_state volume config.yaml at $cfg failed to parse (see the boid error above)." >&2
	echo "deploy-container: fix: update it (boid config set sandbox.backend container against a running daemon, or edit $cfg directly inside the volume) or remove the boid_state volume to fresh-start (this DISCARDS all daemon state)." >&2
	exit 1
fi
if [ "$backend" != "container" ]; then
	echo "deploy-container: ERROR: the boid_state volume config.yaml resolves sandbox.backend to [$backend], not container." >&2
	echo "deploy-container: an existing config.yaml was preserved above (not overwritten) but does not select the container backend, so the daemon about to start would silently run userns instead of container." >&2
	echo "deploy-container: fix: update it (boid config set sandbox.backend container against a running daemon, or edit $cfg directly inside the volume) or remove the boid_state volume to fresh-start (this DISCARDS all daemon state)." >&2
	exit 1
fi
echo "deploy-container: confirmed effective sandbox.backend=container" >&2
'

echo "deploy-container: stopping any existing compose stack (explicit down before up — see this script's own header comment on why no restart: policy exists in compose.yml)"
"${COMPOSE_CMD[@]}" down || true

echo "deploy-container: starting the compose stack"
"${COMPOSE_CMD[@]}" up -d

echo "deploy-container: done. compose stack is up (sandbox.backend: container, seeded above)."
