# Phase 6 shared base image
# (docs/plans/phase6-container-backend.md §PR2, §決定 2/3/4).
#
# This is the "共有 base イメージ" daemon and job containers are meant to
# share once the container backend lands (PR5) and gets cutover-gated public
# (PR7). As of PR2 nothing in the dispatcher builds a `docker create` /
# `docker run` from this image yet — it exists purely so the image and its
# entrypoint (`boid runner-container`, internal/sandbox/runner/
# runner_container_linux.go) are buildable and testable ahead of that wiring.
#
# What this image bakes in (決定 2 — everything else stays a runtime mount):
#   - an OS base (debian:bookworm-slim) with ca-certificates for outbound
#     TLS (gateway / egress proxy / registries).
#   - the boid binary itself, at /usr/local/bin/boid (canonical location)
#     with a build-time symlink at /run/boid/bin/boid. The symlink matters:
#     project.yaml host-command shims are declared as *relative* symlinks
#     `<name> -> boid` resolved against their own directory (see
#     internal/dispatcher/sandbox_builder.go's hostCommandSymlinks /
#     sandboxShimBinDir doc comments) — baking "boid" itself at that fixed
#     location is what lets the per-container `<name>` shims (see next
#     bullet) resolve without change to that existing, already-shared
#     symlink-generation logic. This is not one of the "individual shim
#     symlinks" decision 2 forbids baking: "boid" is a fixed name, not a
#     per-project host_commands entry unknown at build time.
#   - the /run/boid/bin directory itself, owned by the --user uid this
#     container runs as (BOID_UID/BOID_GID build args, default 1000:1000 —
#     決定 4's "job container runs `--user <daemon uid>:<gid>`), so the
#     non-root container entrypoint can write the per-project `<name>` shim
#     symlinks into it at container start without needing root.
#   - an /etc/passwd (and /etc/group) entry for that same uid/gid — without
#     one, os/user-dependent tooling inside the sandbox (ssh, some git
#     helpers, `id`, etc.) breaks on an unknown-uid lookup (決定 4's PR2
#     注意点).
#
# What it deliberately does NOT bake in:
#   - individual host-command shim symlinks (/run/boid/bin/<name>) — these
#     are declared per-project (project.yaml host_commands) and unknown at
#     image build time. The container entrypoint materializes them fresh
#     from spec.Symlinks on every container start (決定 2; see
#     runner.applySpecSymlinks' doc comment).
#   - the $HOME toolchain (go/volta/claude/codex/opencode CLIs, credentials)
#     — stays on the workspace volume, unchanged from the userns backend
#     (親ドキュメント container-based-boid.md「ツールはイメージ」の但し書き)。
#   - a PID 1 / init process. The container is expected to be launched with
#     `HostConfig.Init: true` (docker-init/tini), which owns zombie reap and
#     PID 1 signal disposition; SIGUSR1→agent-SIGTERM forwarding is already
#     handled by the harness adapters' own sigutil.ForwardAndWait once a
#     signal reaches this entrypoint process — nothing new to embed here
#     (決定 3).

ARG GO_VERSION=1.24.2
ARG BOID_UID=1000
ARG BOID_GID=1000

# ---- builder ------------------------------------------------------------
FROM docker.io/library/golang:${GO_VERSION}-bookworm AS builder
WORKDIR /src

# Separate module-download layer from the source COPY so an unrelated source
# change doesn't invalidate the (slow) module download cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0: boid's only cgo-shaped dependency (modernc.org/sqlite) is a
# pure-Go/transpiled sqlite implementation, so a static binary is possible —
# preferred for a slim runtime stage since it needs no libc ABI matching
# against the runtime base image's glibc.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /out/boid .

# ---- runtime --------------------------------------------------------------
FROM docker.io/library/debian:bookworm-slim
ARG BOID_UID
ARG BOID_GID

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git \
    && rm -rf /var/lib/apt/lists/*

# The job/daemon uid, matching the host daemon uid (決定 4 — typically
# 1000:1000). Baking the passwd/group entry at image-build time means every
# container started from this image, regardless of which host uid the
# operator's daemon runs as, needs the matching BOID_UID/BOID_GID build args
# — image selection is per-uid, consistent with 決定 2's "boid update = image
# rebuild" model.
RUN groupadd --gid "${BOID_GID}" boid \
    && useradd --uid "${BOID_UID}" --gid "${BOID_GID}" --home-dir /home/boid --shell /bin/bash --create-home boid

COPY --from=builder /out/boid /usr/local/bin/boid

# /run/boid/bin: the shim directory the container entrypoint writes
# per-project host-command symlinks into at container start (spec.Symlinks
# — see runner.RunContainer). The "boid" entry itself is baked (relative
# shim targets resolve against it, see header comment); the per-host-command
# <name> entries are not (decision 2).
RUN mkdir -p /run/boid/bin \
    && ln -s /usr/local/bin/boid /run/boid/bin/boid \
    && chown -R "${BOID_UID}:${BOID_GID}" /run/boid/bin

# /workspace: WORKDIR below would otherwise create this as root even after
# USER boid (a well-known Dockerfile gotcha — WORKDIR's implicit mkdir does
# not honor the active USER), which would make it unwritable by the
# non-root entrypoint on any run that doesn't mount something over it.
# Production job containers always mount a real workspace volume/bind here
# (決定 4/10), but the image's own fallback directory should still be usable
# on its own — e.g. for exactly this kind of standalone image smoke test.
RUN mkdir -p /workspace && chown "${BOID_UID}:${BOID_GID}" /workspace

USER boid
WORKDIR /workspace

# The container entrypoint execs the image-baked boid binary directly by
# absolute path (no PATH-based shim resolution needed, unlike the per-project
# host-command shims) — see runner.RunContainer's doc comment for exactly
# what runner-container does and does not do relative to the userns
# runner-outer/-inner/-inner-child chain.
ENTRYPOINT ["/usr/local/bin/boid", "runner-container"]
