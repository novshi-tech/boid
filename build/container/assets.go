// Package container also embeds compose.yml and Dockerfile (this file) so
// `boid`'s host-mode orchestration (cmd/host.go, BOID_MODE=container — PR-3
// Option 4 host-mode redesign, docs/plans/volume-only-daemon.md §論点c) can
// extract them to a stable filesystem path and locate them even when
// invoked as a standalone-installed binary with no adjacent boid repo
// checkout (round-2 codex review Major 1: "host mode cannot autostart from
// an installed standalone CLI").
//
// This does NOT solve the deeper "build the image from scratch" case:
// Dockerfile's own build context is `COPY . .` — this entire go source
// tree, not just this directory — so embedding what an image BUILD needs
// into the very binary being built from that same source would mean
// embedding a full second copy of the repository inside itself (impractical
// and circular, see cmd/host.go's own header comment, which predates this
// file). These embedded assets exist to enable a narrower, still genuinely
// useful fallback: bringing up the compose stack by PULLING this binary's
// own matching version's GHCR image (docs/plans/release-onboarding.md
// 穴4/PR4 — internal/version.DefaultContainerImage(), rather than requiring
// an already-built local image, which was this fallback's pre-PR4 shape)
// without needing a checkout to be around at all. See cmd/host.go's
// deployFromEmbeddedAssets for where this is used, and findComposeRoot's
// own doc comment for the checkout-based primary path this is a fallback
// FOR.
package container

import "embed"

// compose.podman.override.yml rides along because the no-checkout fallback
// has to be able to deploy on the same engines a checkout can — and on
// rootless podman that means the deploy script needs this overlay present
// beside compose.yml (see that file's own header comment, and
// cmd/host.go's extractComposeAssets, which mirrors this repo's layout for
// exactly that reason).
//
//go:embed compose.yml compose.podman.override.yml Dockerfile
var Assets embed.FS
