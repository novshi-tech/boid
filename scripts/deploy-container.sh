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

# --- --build (docs/plans/release-onboarding.md 穴4/PR4) ---------------------
# The script's own default flipped from "always build locally" to
# "pull-first" as part of PR4: a bare `go install`-only user has no source
# tree to build from at all (this is exactly what makes host mode's
# no-checkout fallback, cmd/host.go's deployFromEmbeddedAssets, viable in
# the first place), and a plain `boid start` should default to pulling the
# published, versioned image rather than silently building an unversioned
# local one. `--build` is the explicit developer backdoor back to the old
# behavior — cmd/host.go's deployFromCheckout (the "a real checkout was
# found" path, i.e. nose's own day-to-day dev workflow) always passes it,
# so that workflow keeps picking up local code changes exactly like before
# this flip. DEPLOY_CONTAINER_BUILD_ONLY=1 (below) also implies it: that
# knob's entire purpose is building an image, so it must build regardless
# of whether the caller separately passed --build.
#
# --down (docs/plans/release-onboarding.md 決定2/PR5): `boid stop`'s
# re-definition (cmd/stop.go) — tear the compose stack down instead of
# bringing it up. Mutually exclusive with --build (there is nothing to
# build on the way down); checked once both flags are parsed, below.
BUILD=0
DOWN=0
for arg in "$@"; do
	case "$arg" in
	--build)
		BUILD=1
		;;
	--down)
		DOWN=1
		;;
	*)
		echo "error: unknown argument: $arg (only --build/--down are accepted)" >&2
		exit 1
		;;
	esac
done
if [[ "$BUILD" == "1" && "$DOWN" == "1" ]]; then
	echo "error: --build and --down are mutually exclusive" >&2
	exit 1
fi
if [[ "${DEPLOY_CONTAINER_BUILD_ONLY:-0}" == "1" ]]; then
	BUILD=1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCKERFILE="$ROOT_DIR/build/container/Dockerfile"
COMPOSE_FILE="$ROOT_DIR/build/container/compose.yml"
# Stacked on COMPOSE_FILE for rootless podman only — see the podman branch
# below, and that file's own header comment.
PODMAN_OVERRIDE_FILE="$ROOT_DIR/build/container/compose.podman.override.yml"

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

# docker_compose_usable() (round-3 codex review Minor 1): `docker version`
# succeeding only proves the docker ENGINE is reachable, not that the
# compose v2 PLUGIN this script's COMPOSE_CMD depends on
# (`docker compose ...`, not the standalone python `docker-compose`) is
# installed alongside it — a docker install with no compose plugin would
# otherwise be selected here and only fail much later, at the actual `up`
# call, when a genuinely usable podman+podman-compose might have been
# sitting right next to it.
docker_compose_usable() {
	command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1
}

# DOWN_ALT_COMPOSE_CMD (docs/plans/release-onboarding.md 決定2/PR5, codex
# round-4 review Major): --down below only tears down COMPOSE_CMD's own
# engine — the SAME preference-ordered selection this block always runs,
# docker-if-usable-else-podman, independent of which engine actually
# brought the stack up. If the environment changed between the `up` that
# started it and the `boid stop` (--down) that is supposed to stop it
# (docker newly installed, podman.socket newly enabled, ...), --down could
# silently down an EMPTY docker-side compose project while the real
# podman-side stack (or vice versa) keeps running, yet still report
# success. When --down is requested and BOTH engines are usable, this is
# populated with the non-primary engine's compose invocation so the DOWN
# block (below) can tear down both — whichever one actually has the live
# stack gets stopped either way, at the cost of a harmless no-op `down` on
# the other.
DOWN_ALT_COMPOSE_CMD=()

if usable docker && docker_compose_usable; then
	ENGINE=docker
	BUILD_CMD=(docker build)
	COMPOSE_CMD=(docker compose -f "$COMPOSE_FILE")
	if [[ "$DOWN" == "1" ]] && usable podman && command -v podman-compose >/dev/null 2>&1; then
		DOWN_ALT_COMPOSE_CMD=(podman-compose -f "$COMPOSE_FILE")
	fi
elif usable podman; then
	ENGINE=podman
	BUILD_CMD=(podman build)
	# No DOWN_ALT_COMPOSE_CMD computation here: reaching this branch at
	# all already means "usable docker && docker_compose_usable" (the
	# prior branch's condition) evaluated false a few lines up, in this
	# SAME invocation — re-checking it again here cannot yield a
	# different answer, so there is no drift to guard against within one
	# script run. Only the docker-primary branch above needs the
	# alt-engine check.
	if command -v podman-compose >/dev/null 2>&1; then
		COMPOSE_CMD=(podman-compose -f "$COMPOSE_FILE")
	else
		COMPOSE_CMD=()
		echo "warning: podman found but no podman-compose; skipping the compose up/down step (image build only)" >&2
	fi
elif command -v docker >/dev/null 2>&1; then
	echo "error: docker is on PATH but not usable (either 'docker version' failed — daemon not running/unreachable — or the compose v2 plugin ('docker compose version') is missing), and no usable podman was found either" >&2
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
	#
	# --down (docs/plans/release-onboarding.md 決定2/PR5, codex round-1
	# review Major 4): this preflight is a precondition for BRINGING THE
	# STACK UP (the DooD bind mount `up` is about to create), not for
	# tearing it down — `compose down`/`podman-compose down` only removes
	# already-existing containers/networks and does not itself need
	# podman.socket's docker-API-compatible listener at all. Gating it
	# unconditionally used to mean `boid stop` (this script's own --down)
	# could never succeed at recovering from EXACTLY the failure mode this
	# check exists to catch — a stopped/never-enabled podman.socket — since
	# the preflight refused before ever reaching the down logic below.
	if [[ "$DOWN" != "1" ]] && ! systemctl --user is-active podman.socket >/dev/null 2>&1; then
		echo "error: podman.socket is not active — required for the DooD engine-socket bind (BOID_DOCKER_SOCK_SRC=$BOID_DOCKER_SOCK_SRC)." >&2
		echo "  fix: systemctl --user enable --now podman.socket" >&2
		exit 1
	fi

	# Rootless podman needs the daemon container created with
	# `userns_mode: keep-id`, or its in-container uid maps to a host subuid
	# that owns none of the host paths compose.yml bind-mounts in — the
	# daemon then dies on its own broker socket bind (2026-07-26 dogfood;
	# build/container/compose.podman.override.yml's header comment has the
	# full symptom list). That value is podman-only — `docker compose`
	# rejects it — so it lives in a separate overlay stacked on here rather
	# than in compose.yml itself.
	#
	# Gated on rootless specifically because podman REFUSES keep-id for
	# containers created by root: emitting it unconditionally would turn a
	# working rootful-podman deploy into a hard create failure. The same
	# rootless-only reasoning is applied independently, against the docker
	# API rather than this CLI, for JOB containers
	# (internal/dispatcher/container_backend_userns.go).
	if [[ ${#COMPOSE_CMD[@]} -gt 0 ]]; then
		if [[ "$(podman info --format '{{.Host.Security.Rootless}}' 2>/dev/null)" == "true" ]]; then
			echo "deploy-container: rootless podman — layering $PODMAN_OVERRIDE_FILE (userns keep-id)"
			COMPOSE_CMD+=(-f "$PODMAN_OVERRIDE_FILE")
		else
			echo "deploy-container: rootful podman — skipping the keep-id overlay (podman rejects keep-id for root-created containers)"
		fi
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

# --- --down: tear the stack down and exit, before any build/pull/up step ---
# `boid stop` (cmd/stop.go, docs/plans/release-onboarding.md 決定2/PR5)
# invokes this script with --down instead of driving `docker/podman
# compose down` directly — same single-source-of-truth rationale as
# --build/up (this file's own header comment): engine detection, the
# podman keep-id overlay, and BOID_IMAGE all have to agree with whatever
# `up` last used, or compose could resolve a different project/volume
# identity for the down call than the one actually running.
if [[ "$DOWN" == "1" ]]; then
	if [[ ${#COMPOSE_CMD[@]} -eq 0 ]]; then
		echo "error: podman-compose is required to bring the compose stack down; install podman-compose" >&2
		exit 1
	fi
	# BOID_IMAGE is unset here (deliberately no build/pull step ran) —
	# `compose down` never resolves `image:`, only service/network/volume
	# names, so this is safe; export a placeholder so compose does not warn
	# about an unset interpolation variable.
	: "${BOID_IMAGE:=unused}"
	export BOID_IMAGE
	# codex round-4/round-5 review Major: under `set -e`, a bare
	# `"${COMPOSE_CMD[@]}" down` that FAILS aborts the script immediately
	# — the DOWN_ALT_COMPOSE_CMD attempt below would never even run, so a
	# primary-engine down failure could never be masked by a successful
	# alternate-engine down (the exact scenario DOWN_ALT_COMPOSE_CMD
	# exists for: the stack was actually started under the OTHER engine).
	# Guarding each attempt with `if ! ...; then` (rather than a bare
	# command) keeps `set -e` from firing on either one individually, so
	# both always get a chance to run; PRIMARY_OK/ALT_OK below then decide
	# the overall outcome explicitly instead of silently reporting success
	# on a down that never happened.
	echo "deploy-container: --down — stopping the compose stack (engine=$ENGINE)"
	PRIMARY_OK=1
	if ! "${COMPOSE_CMD[@]}" down; then
		PRIMARY_OK=0
		echo "warning: compose down failed for engine=$ENGINE" >&2
	fi
	# DOWN_ALT_COMPOSE_CMD (codex round-4 review Major, this file's own
	# header comment on the variable): best-effort teardown of the OTHER
	# engine too, in case the stack was actually brought up under it
	# (docker/podman availability drifted between `up` and this `down`).
	# ALT_ATTEMPTED/ALT_OK (codex round-6 review Major): tracked as TWO
	# separate flags, not one "ok unless it failed" flag defaulting to
	# success — the single-engine case (no alternate engine usable at all,
	# DOWN_ALT_COMPOSE_CMD empty) must NOT count as "the alternate
	# succeeded" when deciding the overall outcome below, or a primary-
	# engine failure on an ordinary single-engine host would be silently
	# reported as success (there is no alternate to have masked it with).
	ALT_ATTEMPTED=0
	ALT_OK=1
	if [[ ${#DOWN_ALT_COMPOSE_CMD[@]} -gt 0 ]]; then
		ALT_ATTEMPTED=1
		echo "deploy-container: --down — also stopping via the alternate engine, in case the stack was started under it"
		if ! "${DOWN_ALT_COMPOSE_CMD[@]}" down; then
			ALT_OK=0
			echo "warning: compose down failed for the alternate engine too" >&2
		fi
	fi
	# Report failure whenever primary failed AND either no alternate was
	# even attempted (the ordinary single-engine case), or the alternate
	# ALSO failed — an empty/never-created compose project on either
	# engine is expected to no-op successfully, which is not a failure;
	# only "every engine actually attempted came back failed" means the
	# stack might still be running.
	if [[ $PRIMARY_OK -eq 0 ]] && { [[ $ALT_ATTEMPTED -eq 0 ]] || [[ $ALT_OK -eq 0 ]]; }; then
		echo "error: compose down failed on every engine attempted; the stack may still be running" >&2
		exit 1
	fi
	echo "deploy-container: done. compose stack is down."
	exit 0
fi

# --- build (--build/DEPLOY_CONTAINER_BUILD_ONLY) or pull (default) --------
# docs/plans/release-onboarding.md 穴4/PR4: BUILD was computed above from
# --build / DEPLOY_CONTAINER_BUILD_ONLY. Building requires a real checkout
# (`docker build ... "$ROOT_DIR"` needs Dockerfile's `COPY . .` context, and
# `git -C "$ROOT_DIR" rev-parse HEAD` needs a `.git` — neither exists under
# cmd/host.go's embedded-assets fallback, which is exactly why that path
# never passes --build (see this script's own --build comment above) — so
# only the BUILD branch below ever touches git/BUILD_CMD.
if [[ "$BUILD" == "1" ]]; then
	IMAGE_TAG="boid:$(git -C "$ROOT_DIR" rev-parse HEAD)"

	# No --build-arg BOID_UID/BOID_GID here (removed, PR2): the image is
	# arbitrary-uid as of docs/plans/release-onboarding.md 決定1 — it no
	# longer takes a per-uid build arg at all (build/container/Dockerfile
	# dropped both ARGs along with the useradd that consumed them), so a
	# single built image works for any operator uid via compose.yml's
	# `user: "${BOID_UID}:0"` at RUN time instead.
	#
	# BOID_VERSION (docs/plans/release-onboarding.md 穴2, PR1): only an EXACT
	# release tag counts as a version identity worth baking in — see
	# internal/version.IsExactRelease's doc comment for why the rule is this
	# narrow. A dev checkout that is not sitting exactly on a tag (the common
	# case) leaves this empty, and the Dockerfile's ARG BOID_VERSION="" default
	# takes over, producing a binary that honestly reports itself as a local
	# build (internal/version.Version() with no ldflags override falls back to
	# debug.ReadBuildInfo(), which is "(devel)"-shaped for this build path
	# regardless, since .dockerignore excludes .git/ from the build context).
	#
	# `git describe --tags --exact-match` only proves SOME tag sits on HEAD —
	# it says nothing about that tag's shape (codex review of this PR): a
	# "nightly" tag, a "v0.0.14-rc1" pre-release tag, or (with multiple tags
	# on the same commit) an arbitrary pick among them would all pass through
	# unchecked and get baked in as if they were the release identity
	# internal/version.IsExactRelease() actually gates on. Re-validate against
	# the exact same "vMAJOR.MINOR.PATCH" shape here and discard anything that
	# doesn't match, so Version() on the built image agrees with what
	# IsExactRelease() would decide from the tag alone.
	BOID_VERSION="$(git -C "$ROOT_DIR" describe --tags --exact-match 2>/dev/null || true)"
	if ! [[ "$BOID_VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
		BOID_VERSION=""
	fi

	echo "deploy-container: --build — building $IMAGE_TAG from $DOCKERFILE (BOID_VERSION=${BOID_VERSION:-<none>})"
	"${BUILD_CMD[@]}" \
		--build-arg "BOID_VERSION=$BOID_VERSION" \
		-t "$IMAGE_TAG" \
		-t boid-runner:latest \
		-f "$DOCKERFILE" \
		"$ROOT_DIR"

	# The just-built local tag, not whatever BOID_IMAGE this process may
	# have inherited (e.g. a stale value exported by a previous run) —
	# --build's whole point is "run what I just built", not silently
	# pulling something else instead.
	BOID_IMAGE="boid-runner:latest"
else
	IMAGE_TAG="(none — pull-first default, no local build)"

	# Pull-first default: an already-exported BOID_IMAGE wins outright
	# (cmd/host.go's deployFromEmbeddedAssets computes this via
	# internal/version.DefaultContainerImage() in Go — the exact version
	# identity of the running CLI binary, which is strictly better
	# information than anything this script can derive on its own when
	# there is no git checkout to inspect). Otherwise, when a real checkout
	# IS present (e.g. this script invoked directly, with no --build, by a
	# developer who just wants to run the published image instead of
	# building) and happens to sit exactly on a release tag, prefer that
	# release's own GHCR ref — matching internal/version.IsExactRelease's
	# rule and the same tag .github/workflows/blackbox-e2e.yml's publish
	# step pushes for it. Falling back all the way to the bare GHCR
	# "latest" ref otherwise mirrors build/container/compose.yml's own
	# static default (its `image:` line's `${BOID_IMAGE:-...}` fallback).
	if [[ -z "${BOID_IMAGE:-}" ]]; then
		if git -C "$ROOT_DIR" rev-parse --git-dir >/dev/null 2>&1; then
			checkout_tag="$(git -C "$ROOT_DIR" describe --tags --exact-match 2>/dev/null || true)"
			if [[ "$checkout_tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
				BOID_IMAGE="ghcr.io/novshi-tech/boid-runner:$checkout_tag"
			fi
		fi
		: "${BOID_IMAGE:=ghcr.io/novshi-tech/boid-runner:latest}"
	fi
	echo "deploy-container: pull-first default — compose will pull $BOID_IMAGE (pass --build to build locally instead)"
fi
export BOID_IMAGE

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
	echo "error: podman-compose is required to bring the compose stack up (image ready: $BOID_IMAGE, but compose up was skipped — see warning above); install podman-compose or set DEPLOY_CONTAINER_BUILD_ONLY=1 if you only wanted the image" >&2
	exit 1
fi

# --- pre-provision the still-bind-mounted runtime dir -----------------------
# BOID_RUNTIME_DIR must exist and be owned by BOID_UID:BOID_GID BEFORE
# `compose up` (Major 13, PR6 codex review, narrowed to just this one
# directory now that BOID_DATA_DIR/BOID_CONFIG_DIR are named volumes the
# daemon itself owns and no longer need host pre-provisioning): compose/
# docker/podman auto-create a missing bind-mount host path, but as root
# (or whichever uid runs the engine daemon) — the non-root daemon process
# (user: ${BOID_UID}:0 in compose.yml, arbitrary-uid as of docs/plans/
# release-onboarding.md 決定1/PR2 — the HOST bind-mount target below still
# needs a real BOID_UID:BOID_GID chown, independent of the container's own
# gid-0 convention) would then be unable to even see a live socket under
# BOID_RUNTIME_DIR on a genuinely first-ever run against a fresh layout.
# chown is best-effort (a warning, not
# fatal): it fails harmlessly when this script is not running as the
# target uid/gid and lacks permission to chown to it (e.g. BOID_UID
# overridden to something other than the invoking user) — in that case
# the directory most likely already has the right ownership (created
# by/for that uid outside this script) and this is a no-op.
echo "deploy-container: ensuring BOID_RUNTIME_DIR ($BOID_RUNTIME_DIR) exists and is owned by ${BOID_UID}:${BOID_GID}"
mkdir -p "$BOID_RUNTIME_DIR"
chown "$BOID_UID:$BOID_GID" "$BOID_RUNTIME_DIR" 2>/dev/null || \
	echo "warning: could not chown $BOID_RUNTIME_DIR to ${BOID_UID}:${BOID_GID} (continuing — it may already be owned correctly)" >&2

# --- config.yaml seed / effective-backend validation: removed (PR-4) ----
# docs/plans/volume-only-daemon.md §論点e: container is now the only
# sandbox backend (the userns backend and the sandbox.backend config key
# that used to select between the two are both gone), so there is nothing
# left to seed into a fresh boid_state volume's config.yaml before first
# boot, and nothing left to validate before starting the compose stack —
# every deployment runs the container backend unconditionally, by
# construction (internal/server/wire.go's buildRuntime), not by config.
# `boid config effective-backend` (cmd/config.go), this step's sole
# caller, was removed in the same PR.

echo "deploy-container: stopping any existing compose stack (explicit down before up — see this script's own header comment on why no restart: policy exists in compose.yml)"
"${COMPOSE_CMD[@]}" down || true

echo "deploy-container: starting the compose stack"
"${COMPOSE_CMD[@]}" up -d

echo "deploy-container: done. compose stack is up (container backend)."
