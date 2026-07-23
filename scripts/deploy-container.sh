#!/usr/bin/env bash
#
# scripts/deploy-container.sh
#
# Phase 6 PR6 skeleton (docs/plans/phase6-container-backend.md §PR6's
# "deploy 機構 (イメージビルド → daemon 再起動) を整備" item). Builds the shared
# base image (build/container/Dockerfile) and (re)starts the
# build/container/compose.yml daemon stack against it.
#
# ############################################################################
# # DO NOT RUN THIS AGAINST A REAL ~/.local/share/boid / ~/.config/boid YET. #
# ############################################################################
# docs/plans/phase6-container-backend.md §PR6 is explicit: "PR6 の compose
# スタックを本番 deploy してはならない" — startup reap / Wait single-ownership /
# a persistent transcript spool (all PR7) are not in place yet, so an
# unattended restart of this stack over still-running sibling job containers
# double-executes them and can lose log data (codex Major 11). This script
# exists so the build+deploy MECHANISM is exercised and testable ahead of
# that cutover — not to be run against production data before it lands.
#
# host has no docker engine, only podman (CLAUDE.md / this plan doc's own
# 前提 note as of 2026-07-22) — the `docker`-branch below is exactly what
# CI (docs/plans/phase6-container-backend.md §PR9's e2e-container job, on a
# real-docker ubuntu-24.04 runner) exercises; the `podman`/`podman-compose`
# fallback lets this script also do something useful on today's dev host,
# but is not the plan's target engine (决定, 前提 note: "cutover 前に docker
# engine を導入する").

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DOCKERFILE="$ROOT_DIR/build/container/Dockerfile"
COMPOSE_FILE="$ROOT_DIR/build/container/compose.yml"

# --- select an engine -------------------------------------------------------
# Prefers docker (the plan's target — compose v2 syntax, DOCKER_HOST semantics
# dockerproxy/containerBackend are written against). Falls back to podman +
# podman-compose only because that is what today's dev host actually has;
# `docker compose` (the reference implementation) is what §PR9's CI job uses.
if command -v docker >/dev/null 2>&1; then
	ENGINE=docker
	BUILD_CMD=(docker build)
	COMPOSE_CMD=(docker compose -f "$COMPOSE_FILE")
elif command -v podman >/dev/null 2>&1; then
	ENGINE=podman
	BUILD_CMD=(podman build)
	if command -v podman-compose >/dev/null 2>&1; then
		COMPOSE_CMD=(podman-compose -f "$COMPOSE_FILE")
	else
		COMPOSE_CMD=()
		echo "warning: podman found but no podman-compose; skipping the compose up/down step (image build only)" >&2
	fi
else
	echo "error: neither docker nor podman found on PATH" >&2
	exit 1
fi
echo "deploy-container: using engine=$ENGINE"

# --- compute the required compose env vars -----------------------------
# Mirrors cmd/start.go's default*Dir/*Path XDG-or-fallback convention exactly
# (see build/container/.env.example's own comments on why this is computed
# in bash rather than left to compose's own interpolation).
#
# XDG_DATA_HOME/XDG_CONFIG_HOME are resolved FIRST, and BOID_DATA_DIR/
# BOID_CONFIG_DIR are always derived from them — not independently
# overridable (Major 10, PR6 codex review): compose.yml's `environment:`
# block passes these same XDG_DATA_HOME/XDG_CONFIG_HOME values into the
# container so cmd/start.go's own default*Dir/*Path helpers (and Go's
# os.UserConfigDir()) resolve to exactly where the bind mount's source
# (== target, also Major 10) actually is. Overriding BOID_DATA_DIR alone,
# independently of XDG_DATA_HOME, would desync the bind-mount source from
# what the daemon resolves internally — set XDG_DATA_HOME/XDG_CONFIG_HOME
# instead for a non-default layout.
: "${XDG_DATA_HOME:=$HOME/.local/share}"
: "${XDG_CONFIG_HOME:=$HOME/.config}"
BOID_DATA_DIR="$XDG_DATA_HOME/boid"
BOID_CONFIG_DIR="$XDG_CONFIG_HOME/boid"
# BOID_RUNTIME_DIR mirrors internal/client.DefaultSocketPath()'s exact
# fallback chain, not just its XDG_RUNTIME_DIR-or-/run/user/<uid> shape
# (Major 12, PR6 codex review): DefaultSocketPath only uses
# /run/user/<uid> when os.Stat confirms that directory actually exists on
# THIS host — it is not systemd-logind-managed on every host (a headless
# server with no active login session, some minimal container base
# images, ...) — falling back to a bare /tmp/boid-<uid>.sock file
# otherwise. The pre-fix line here used /run/user/<uid> unconditionally,
# silently diverging from what a bare `boid start` on the SAME host
# resolves to whenever that directory doesn't exist — breaking §決定4's
# "server socket の host 同一 path bind (相互排他)" contract in exactly the
# case it matters most (this stack's whole reason to run resolving a
# DIFFERENT socket path than the host daemon it's meant to coexist with
# / roll back to, so both start "successfully" as two live daemons at
# once). DefaultSocketPath's own BOID_SOCKET override (an arbitrary full
# path, not a directory) has no bind-mountable-directory equivalent here
# and is intentionally not replicated — an operator using it must set
# BOID_RUNTIME_DIR (or BOID_SOCKET's own containing directory) manually.
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
# permission to open /var/run/docker.sock (DooD). `getent group docker`
# is the portable way to look this up (works whether the group entry
# comes from /etc/group or an NSS backend); if the host has no `docker`
# group at all (e.g. a podman-only host with no docker-shaped group,
# CLAUDE.md's noted dev-host state), fall back to compose.yml's own
# `${DOCKER_GID:-999}" default rather than failing here — group_add with
# a GID that doesn't exist on this host is harmless (docker/podman does
# not validate it against /etc/group), and 999 is podman-compose 1.0.6's
# own requirement (an unset var used in a list context fails
# interpolation on some versions) as well as a common `docker.io`
# package default.
: "${DOCKER_GID:=$(getent group docker 2>/dev/null | cut -d: -f3)}"
: "${DOCKER_GID:=999}"
export BOID_DATA_DIR BOID_CONFIG_DIR BOID_RUNTIME_DIR BOID_UID BOID_GID DOCKER_GID XDG_DATA_HOME XDG_CONFIG_HOME

IMAGE_TAG="boid:$(git -C "$ROOT_DIR" rev-parse HEAD)"

echo "deploy-container: building $IMAGE_TAG from $DOCKERFILE"
"${BUILD_CMD[@]}" \
	--build-arg "BOID_UID=$BOID_UID" \
	--build-arg "BOID_GID=$BOID_GID" \
	-t "$IMAGE_TAG" \
	-t boid-runner:latest \
	-f "$DOCKERFILE" \
	"$ROOT_DIR"

if [[ ${#COMPOSE_CMD[@]} -eq 0 ]]; then
	echo "deploy-container: image built ($IMAGE_TAG); compose up skipped (see warning above)"
	exit 0
fi

echo "deploy-container: stopping any existing compose stack (explicit down before up — see this script's own header comment on why no restart: policy exists in compose.yml)"
"${COMPOSE_CMD[@]}" down || true

echo "deploy-container: starting the compose stack"
"${COMPOSE_CMD[@]}" up -d

echo "deploy-container: done. NOTE: this stack is a PR6 skeleton — see this script's header comment before pointing it at real data."
