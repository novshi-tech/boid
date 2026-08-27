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
#   - the /run/boid/bin directory itself, made writable by ANY uid running
#     with supplementary group 0 (決定 4's "job container runs `--user
#     <daemon uid>:<gid>`", combined with docs/plans/
#     release-onboarding.md 決定1's arbitrary-uid redesign — see below), so
#     the non-root container entrypoint can write the per-project `<name>`
#     shim symlinks into it at container start without needing root,
#     regardless of which uid the daemon's own host actually runs as.
#
# 決定1 (docs/plans/release-onboarding.md, "OpenShift 流 arbitrary-uid",
# PR2): through PR2's predecessor this image took BOID_UID/BOID_GID build
# args and useradd'd exactly one fixed uid/gid pair — every operator whose
# host uid differed from the image's baked-in default had to rebuild the
# image themselves before `docker run --user <their-uid>:<their-gid>` would
# even find a passwd entry, let alone own anything. That is incompatible
# with a single publicly-published image (決定1's whole point — see
# docs/plans/release-onboarding.md's 背景).
#
# The fix is the OpenShift convention: gid 0 ("root group", but NOT root
# privilege — nothing here grants CAP_* or bypasses DAC, it is an ordinary
# group like any other) plus `chmod g=u` on every directory the runtime
# needs to write. Concretely below:
#   - /etc/passwd and /etc/group get `chmod g=u` so the RUNTIME self-
#     registration snippet (internal/selfuser package,
#     cmd/start.go / cmd/runner_container.go / the workspace-init wrapper —
#     see their own doc comments) can append an entry for whatever uid it
#     is actually running as, despite that uid not owning either file.
#     There is no more build-time /etc/passwd entry at all — the uid is
#     unknown until `docker run --user <uid>:0` supplies it.
#   - /home/boid, /run/boid/bin, and /workspace are `chgrp -R 0` +
#     `chmod -R g=u`: any uid started with `--user <uid>:0` gets exactly
#     the same read/write/execute bits its OWNING uid (1000, the image's
#     placeholder USER below) would have had, because gid 0 membership now
#     carries the owning uid's own permission bits.
#   - USER is `1000:0`, not `1000:1000` — the second number is genuinely
#     "which group", not "which uid to also pretend to be". A real deploy's
#     compose `user: "<uid>:0"` (build/container/compose.yml) overrides
#     this at container-start time to whatever uid the operator's daemon
#     actually runs as; the baked `1000:0` only matters for `docker run`
#     invocations that never override --user at all (a standalone image
#     smoke test, as one of this file's later comments notes).
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

# BOID_VERSION (docs/plans/release-onboarding.md 穴2, PR1): this image's
# build context is `COPY . .` below, and .dockerignore excludes .git/, so a
# plain `go build` in the builder stage below has no VCS info to stamp and
# reports "(devel)" via debug.ReadBuildInfo() — internal/version.Version()'s
# go-install fallback path has nothing to fall back TO here. The caller
# (scripts/deploy-container.sh, .github/workflows/blackbox-e2e.yml) passes
# the real version as a build-arg instead, and it gets baked in below via
# -ldflags -X rather than relying on VCS stamping. Left unset (empty
# string), the ldflags -X below sets buildVersion to "" — internal/version.
# Version() treats that identically to no override at all, i.e. the
# "(devel)"-shaped fallback, so an unversioned image build (a bare
# `docker build` with no --build-arg) still produces a working binary that
# honestly reports itself as a local build rather than silently mis-tagging.
ARG BOID_VERSION=""

# ---- builder ------------------------------------------------------------
FROM docker.io/library/golang:${GO_VERSION}-bookworm AS builder
WORKDIR /src

# Separate module-download layer from the source COPY so an unrelated source
# change doesn't invalidate the (slow) module download cache.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG BOID_VERSION

# CGO_ENABLED=0: boid's only cgo-shaped dependency (modernc.org/sqlite) is a
# pure-Go/transpiled sqlite implementation, so a static binary is possible —
# preferred for a slim runtime stage since it needs no libc ABI matching
# against the runtime base image's glibc.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-X github.com/novshi-tech/boid/internal/version.buildVersion=${BOID_VERSION}" \
    -o /out/boid .

# ---- runtime --------------------------------------------------------------
FROM docker.io/library/debian:bookworm-slim

# The runner protocol this image speaks. A workspace's `container_image`
# override must carry this exact label/value or containerBackend.resolveImage
# refuses to launch from it (internal/dispatcher/container_backend.go's
# boidRunnerProtocolLabel / boidRunnerProtocolVersion, §決定 11).
#
# What the check does and does not buy, stated plainly because the constant's
# own comment used to overstate it: this is a COMPATIBILITY DECLARATION, not
# a provenance proof. Anyone can put this label on any image — three lines of
# Dockerfile — so it establishes nothing about where the image came from. What
# it does catch is the mistake that actually happens: an override pointed at
# an image that was never meant to be a boid runner (a base language image, a
# leftover app image, a typo'd tag), which would otherwise fail deep inside
# the job with a confusing error instead of at launch with a clear one.
# Anything stronger — signatures, a registry allowlist — is a separate
# mechanism, not a stricter reading of this one.
#
# Bump the value only when the runner contract itself changes incompatibly,
# and bump boidRunnerProtocolVersion in the same commit; a Go unit test pins
# that these two agree, and the image-build CI job pins that a BUILT image
# really carries it.
LABEL boid.runner_protocol="v1"

# gh CLI version pin (Minor 14, PR6 codex review): `apt-get install gh`
# with no version pin picks up whatever GitHub's apt repo currently
# publishes as `stable`, so two builds of this same Dockerfile on
# different days can silently produce different gh versions — bump this
# deliberately (`apt-cache madison gh` against
# https://cli.github.com/packages, after adding its repo, lists the exact
# version string apt expects) rather than floating on latest.
# Note that GitHub's apt repo keeps ONLY the current release — it does not
# archive older ones. A pin therefore stops building the moment upstream
# publishes the next version, so this needs a deliberate bump on that cadence
# rather than only when we want a newer gh. It has now happened twice:
#
#   - 2.96.0 vanished on 2026-07-31
#   - 2.97.0 vanished on 2026-08-21, hours after upstream published 2.98.0
#     (2026-08-20 22:15 UTC) — every branch including main stopped building
#
# This bump is deliberate stopgap, not the fix. nose の判断 (2026-08-21):
# **gh はこのイメージから退役させ、GitHub API は API gateway (`services:` に
# github-api を足す) に倒す** —— pin が腐る問題ごと無くなる。順序があるので
# 分けてある: 先に Dockerfile から消すと、サンドボックス内で PR を作る経路
# (internal/skills/data/boid-task の PR 手順、dev-pr-flow スキル) が死ぬ。
# gateway 側の service 追加とスキルの書き換えが済んでから削る。
#
# もし退役より先にまた apt から消えたら、その場しのぎで bump するより
# GitHub Releases の tarball
# (https://github.com/cli/cli/releases/download/v${GH_VERSION}/gh_${GH_VERSION}_linux_$(dpkg --print-architecture).tar.gz)
# へ切り替える方が安い —— Releases は古いバージョンを永続保持するので pin が
# 腐らず、pin 本来の目的 (再現性) は apt より強く満たされる。
ARG GH_VERSION=2.98.0

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates git curl gpg jq \
    && rm -rf /var/lib/apt/lists/*

# Native runtime dependencies for workspace-HOME-installed toolchains.
# Per the header's 決定 2 note, the toolchains themselves stay on the
# workspace volume and are user-installed into $HOME without root
# (dotnet-install.sh → ~/.dotnet, `npx playwright install chromium` →
# ~/.cache/ms-playwright) — but their shared-library dependencies can only
# come from apt, which jobs (non-root, no sudo) cannot run. Those, and only
# those, are baked here:
#   - libicu72 + tzdata: the .NET runtime hard-requires ICU for
#     globalization and aborts at startup without it. The
#     DOTNET_SYSTEM_GLOBALIZATION_INVARIANT=1 escape hatch is not an
#     alternative — it breaks ja-JP date/currency formatting.
#   - libasound2 … libxrandr2: Playwright chromium's Debian 12 shared-lib
#     set (what `playwright install-deps chromium` would apt-install; that
#     command itself needs root, so it cannot run inside a job).
#   - fonts-noto-cjk: CJK glyphs — without them, Japanese pages render as
#     tofu (□) in screenshots.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        libicu72 tzdata \
        libasound2 libatk-bridge2.0-0 libatk1.0-0 libatspi2.0-0 \
        libcairo2 libcups2 libdbus-1-3 libdrm2 libgbm1 libglib2.0-0 \
        libnspr4 libnss3 libpango-1.0-0 libx11-6 libxcb1 libxcomposite1 \
        libxdamage1 libxext6 libxfixes3 libxkbcommon0 libxrandr2 \
        fonts-noto-cjk \
    && rm -rf /var/lib/apt/lists/*

# broker host-command provisioning (docs/plans/phase6-container-backend.md
# §PR6, §決定4): once the daemon runs in this image (compose deploy, DooD
# model), the "host" that host_commands resolve against IS this image — a
# host command the broker execs directly (host_commands.go /
# ResolveHostCommands, no shim/sandbox involved) has to actually be
# installed here, not just declared in some project's host_commands.yaml.
#
#   - gh (GitHub CLI): installed from GitHub's own apt repo below (Debian's
#     default repos do not carry it), pinned to GH_VERSION (Minor 14, PR6
#     codex review) so the image is reproducible across builds instead of
#     picking up whatever `stable` happens to publish that day.
#   - project-local scripts (a host_commands entry with an explicit
#     project-relative `path:`) are NOT baked in here — §決定4 is explicit
#     that these "come from the project side", not the image. §決定4 also
#     forbids baking credentials into the image (secret_store's env
#     injection / volume-mounted credential files continue to be the only
#     paths for those, unchanged from the userns backend).
#   - any other host-provisioned tool (e.g. a hypothetical company-internal
#     CLI with no public package) is expected to arrive via a host bind
#     mount over /opt/boid-host-tools rather than an apt/curl install baked
#     into this shared image — the directory below is the mount point
#     skeleton only; nothing populates it at build time. The compose
#     daemon service (build/container/compose.yml) is where a real
#     deployment would bind-mount such a tool's actual host directory onto
#     this path and add it to PATH via host_commands.<name>.path.
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends "gh=${GH_VERSION}" \
    && rm -rf /var/lib/apt/lists/*

# python3 runtime for pip-distributed host_commands (e.g. az CLI, which has
# no standalone/single-binary distribution — it is a pip package and needs a
# python3 interpreter to run at all). Unlike gh above, the tool itself is
# NOT installed here: only the interpreter/pip/venv toolchain, which is a
# shared-library-shaped dependency in the same sense as the .NET/Playwright
# native libs earlier in this file. The actual `pip install` (into a venv
# under $HOME, e.g. ~/host-tools/<name>-venv) happens against the daemon's
# boid_state volume — the same "runtime stays on the persistent volume,
# only its native deps are baked" split every other host-provisioned
# toolchain in this image follows. python3-venv is required rather than
# relying on `pip install --user`: Debian 12's python3-pip enforces
# PEP 668 (externally-managed-environment) and refuses a bare pip install,
# venv is the standard way around that.
RUN apt-get update \
    && apt-get install -y --no-install-recommends python3 python3-venv python3-pip \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /opt/boid-host-tools

# Integration Pack registry (docs/plans/signal-ingest-detailed-design.md
# §6.1/§6.2): the daemon's `integrations.dir` (default /opt/boid/integrations)
# is enumerated once at startup for installed Packs (internal/integrationpack.
# LoadPacks). boid-api-skills is a separate public repo from this one, so the
# official Packs (jira-cloud/bitbucket-cloud/slack) are fetched here via a
# shallow, single-commit clone rather than depending on any host-local
# checkout path (nose directive, 2026-08-27) — this Dockerfile must build
# identically on any host, not just one with boid-api-skills already checked
# out next to this repo. Pinned to a commit SHA, not a floating branch, for
# the same reproducibility reason GH_VERSION above is pinned — bump
# deliberately when a new Pack version needs to ship, and update
# CredentialSlot/serviceProfile config alongside it if the Pack's manifest
# shape changed.
ARG BOID_API_SKILLS_REF=366c7dac8341d6465500f89ea6aa2ed89931a510
RUN git init -q /tmp/boid-api-skills \
    && git -C /tmp/boid-api-skills remote add origin https://github.com/novshi-tech/boid-api-skills.git \
    && git -C /tmp/boid-api-skills fetch --depth 1 origin "$BOID_API_SKILLS_REF" \
    && git -C /tmp/boid-api-skills checkout -q FETCH_HEAD \
    && mkdir -p /opt/boid/integrations \
    && cp -r /tmp/boid-api-skills/jira-cloud /tmp/boid-api-skills/bitbucket-cloud /tmp/boid-api-skills/slack /opt/boid/integrations/ \
    && rm -rf /tmp/boid-api-skills \
    && chgrp -R 0 /opt/boid/integrations && chmod -R g=u /opt/boid/integrations

# Arbitrary-uid (docs/plans/release-onboarding.md 決定1): `chmod g=u` on
# /etc/passwd and /etc/group lets the RUNTIME self-registration snippet
# (internal/selfuser, invoked from cmd/start.go / cmd/runner_container.go /
# the workspace-init wrapper) append an entry for whatever uid actually
# starts the container — despite that uid owning neither file — the same
# way OpenShift's arbitrary-uid images work. There is no build-time
# groupadd/useradd anymore: the uid is unknown until `docker run --user
# <uid>:0` supplies it.
RUN chmod g=u /etc/passwd /etc/group

COPY --from=builder /out/boid /usr/local/bin/boid

# notify.command scripts (docs/plans/ の container 移行に伴う notify 断絶の
# 修正): container backend への移行で daemon プロセス自身がコンテナ内で
# exec するようになった結果、notify.command がホスト専用パス
# (~/.local/bin/ntfy.sh 等) を指す旧設定は container のファイルシステムに
# その実行ファイルが存在せず無言で失敗するようになった。ntfy.sh 通知
# スクリプト (build/container/ntfy.sh) を image に焼き込み、operator は
# config.yaml の notify.command を /usr/local/bin/ntfy.sh に向け直すだけで
# 復旧できるようにする。トピック URL などの秘匿寄りの値はここに焼き込まず
# notify.command 自体の引数 (config.yaml 側、boid_state named volume に
# 永続化) として渡す設計 — 詳細は ntfy.sh 自身のコメント参照。
COPY build/container/ntfy.sh /usr/local/bin/ntfy.sh
RUN chmod +x /usr/local/bin/ntfy.sh

# /run/boid/bin: the shim directory the container entrypoint writes
# per-project host-command symlinks into at container start (spec.Symlinks
# — see runner.RunContainer). The "boid" entry itself is baked (relative
# shim targets resolve against it, see header comment); the per-host-command
# <name> entries are not (decision 2). `chgrp 0` + `chmod g=u` (rather than
# a `chown` to one fixed uid) is what makes this writable by ANY uid the
# arbitrary-uid image is started as, as long as it keeps supplementary
# group 0 (決定1).
RUN mkdir -p /run/boid/bin \
    && ln -s /usr/local/bin/boid /run/boid/bin/boid \
    && chgrp -R 0 /run/boid/bin && chmod -R g=u /run/boid/bin

# /workspace: WORKDIR below would otherwise create this as root even after
# USER 1000:0 (a well-known Dockerfile gotcha — WORKDIR's implicit mkdir
# does not honor the active USER), which would make it unwritable by the
# non-root entrypoint on any run that doesn't mount something over it.
# Production job containers always mount a real workspace volume/bind here
# (決定 4/10), but the image's own fallback directory should still be usable
# on its own — e.g. for exactly this kind of standalone image smoke test.
RUN mkdir -p /workspace && chgrp 0 /workspace && chmod g=u /workspace

# /home/boid/.local/share/boid, /home/boid/.config/boid: pre-create the
# daemon's XDG data/config directories with gid-0/g=u permissions before the
# compose stack mounts named volumes over them (docs/plans/
# volume-only-daemon.md §論点 d). Docker's named-volume "copy from image on
# first mount to an empty volume" behavior includes ownership/mode, so
# preparing these dirs at build time is what makes the daemon (running as
# ANY uid with supplementary group 0, per 決定1) able to create boid.db +
# WAL files, secret.key, web_secret, install_id, and tls/ inside those
# volumes at first boot. Without this, the volumes mount as root:root
# 0755 and a non-1000-uid daemon crashes with SQLITE_CANTOPEN before it can
# log anything — the gid-0/g=u pair is what makes that ownership survive
# a uid that ISN'T the image's own placeholder 1000 (see USER below).
RUN mkdir -p /home/boid/.local/share/boid /home/boid/.config/boid \
    && chgrp -R 0 /home/boid && chmod -R g=u /home/boid

# HOME is set explicitly rather than left to the container runtime's own
# "look up /etc/passwd for --user's uid" fallback (which docker/runc do
# when HOME is unset in the image config): that fallback only works when
# the uid has a passwd entry, and as of 決定1 this image bakes none — the
# entry is self-registered at runtime, AFTER the process (and therefore
# after env resolution) has already started. Without this, a fresh
# container would start with HOME="/" until self-registration ran, and
# os.UserHomeDir() (which reads $HOME directly on Linux, never /etc/passwd)
# would see that stale value for its own first calls.
ENV HOME=/home/boid

# 1000:0 is a placeholder identity for standalone `docker run` invocations
# that never override --user (e.g. an image smoke test) — decision 1 means
# neither number is load-bearing for a real deploy: compose's `user:
# "<uid>:0"` (build/container/compose.yml) always overrides both at
# container-start time to whatever uid the operator's daemon actually runs
# as, keeping gid pinned at 0 so the gid-0/g=u permissions above apply.
USER 1000:0
WORKDIR /workspace

# The container entrypoint execs the image-baked boid binary directly by
# absolute path (no PATH-based shim resolution needed, unlike the per-project
# host-command shims) — see runner.RunContainer's doc comment for exactly
# what runner-container does and does not do relative to the userns
# runner-outer/-inner/-inner-child chain.
ENTRYPOINT ["/usr/local/bin/boid", "runner-container"]
