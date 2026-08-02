// Package scripts embeds deploy-container.sh (this directory's own
// deployment script) so `boid`'s host-mode orchestration (cmd/host.go —
// now the unconditional default, docs/plans/release-onboarding.md
// 決定2/PR5; originally the PR-3 Option 4 host-mode redesign of #835,
// docs/plans/volume-only-daemon.md §論点c) can extract and invoke it even
// when running as a standalone-installed binary with no adjacent boid repo
// checkout (round-2 codex review Major 1: "host mode cannot autostart from
// an installed standalone CLI").
//
// This lives in its own package rather than build/container's own
// assets.go (which embeds compose.yml + Dockerfile) because go:embed
// patterns cannot reference files outside the directory tree rooted at
// the .go file declaring them ("must not contain path elements . or ..",
// per the embed package's own doc) — scripts/ and build/container/ are
// sibling directories, so a single embed.FS cannot cover both without
// physically relocating one into the other (which would break every
// existing "scripts/deploy-container.sh" reference across docs, CI, and
// this repo's own comments, for no real benefit).
//
// round-3 codex review of #835 found that cmd/host.go's embedded-assets
// fallback (deployFromEmbeddedAssets) had drifted from the checkout path
// (scripts/deploy-container.sh via deployFromCheckout): the fallback used
// to run a bare `compose up -d` directly, skipping the script's config
// seed + effective-backend validation, BOID_UID/BOID_GID/DOCKER_GID setup,
// runtime-dir provisioning, and podman socket preflight entirely — a fresh
// no-checkout deploy would silently start the daemon in userns mode
// (unreachable at the CLI listener) instead of container mode. This
// package exists so BOTH paths invoke the exact same script — single
// source of truth for what "deploy the container backend" means, rather
// than reimplementing a second, narrower copy of that logic in Go. See
// cmd/host.go's own header comment for how the two paths now differ only
// in which root directory the script runs from and whether it is allowed
// to build a fresh image.
package scripts

import "embed"

//go:embed deploy-container.sh
var Assets embed.FS
