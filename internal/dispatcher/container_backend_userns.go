package dispatcher

// container_backend_userns.go resolves the userns mode job containers are
// created with, to avoid rootless podman mapping a job container's uid 1000
// to a host subuid that leaves daemon-created bind mounts (e.g. the 0700
// per-workspace HOME) unreadable to the job. See
// container_backend_userns_test.go for the full failure this closes.

import (
	"context"
	"log/slog"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// keepIDUsernsMode is podman's "map the invoking host user to the same uid
// inside the container" userns mode. It is podman-specific: docker has no
// such value (its own UsernsMode.Valid only accepts "" and "host"), and
// podman itself refuses it for containers created by root — hence the
// narrow podman-AND-rootless gate in usernsModeForEngine below.
const keepIDUsernsMode = container.UsernsMode("keep-id")

// rootlessSecurityOption is the token docker-compatible engines report in
// SystemInfo.SecurityOptions when the engine itself runs rootless. Both
// podman and rootless docker use this exact spelling.
const rootlessSecurityOption = "name=rootless"

// usernsModeForEngine decides the UsernsMode for job containers from an
// engine's own self-reported identity. Returns "" — today's behavior, an
// unset field docker and podman alike treat as "default userns" — for every
// engine except rootless podman.
//
// Kept a pure function of the two probe results (rather than reading them
// itself) so the decision table is testable without a fake engine, and so a
// partially-failed probe degrades to the same safe default as a fully
// failed one: a zero-valued ServerVersionResult identifies no engine, and a
// zero-valued SystemInfoResult reports no rootless option, so either one
// alone is enough to fall back to "".
func usernsModeForEngine(version client.ServerVersionResult, info client.SystemInfoResult) container.UsernsMode {
	if !isPodmanEngine(version) || !isRootlessEngine(info) {
		return ""
	}
	return keepIDUsernsMode
}

// isPodmanEngine reports whether a /version response came from podman.
//
// Checks Components first (podman reports a "Podman Engine" entry there,
// docker an "Engine" one) and falls back to Platform.Name, which podman
// fills with a host descriptor like "linux/amd64/ubuntu-24.04" on some
// builds and a product name on others. Components is documented as
// informational and explicitly "not part of the API contract", so treating
// either signal as sufficient — rather than requiring the one this
// dogfood's own podman 4.9.3 happened to use — keeps the detection from
// silently regressing on a future podman that moves its identity between
// the two fields.
func isPodmanEngine(version client.ServerVersionResult) bool {
	if strings.Contains(strings.ToLower(version.Platform.Name), "podman") {
		return true
	}
	for _, component := range version.Components {
		if strings.Contains(strings.ToLower(component.Name), "podman") {
			return true
		}
	}
	return false
}

// isRootlessEngine reports whether a /info response describes a rootless
// engine. SecurityOptions entries are comma-joined key=value lists (e.g.
// "name=seccomp,profile=default"), so each entry is split before matching
// rather than substring-scanned — "name=rootless" is always its own token,
// never a prefix of another option's value.
func isRootlessEngine(info client.SystemInfoResult) bool {
	for _, option := range info.Info.SecurityOptions {
		for _, field := range strings.Split(option, ",") {
			if strings.TrimSpace(field) == rootlessSecurityOption {
				return true
			}
		}
	}
	return false
}

// resolveUsernsMode returns (probing the engine exactly once per backend,
// then caching) the UsernsMode every job container this backend launches is
// created with.
//
// Probe failures are logged and treated as "not rootless podman": dispatch
// continues with today's unset mode rather than failing outright. That is
// the right posture because the fallback is only ever wrong on rootless
// podman, where the job would go on to fail anyway — with its own far more
// specific permission-denied error naming the exact path it could not read,
// which is a better diagnostic than a dispatch refused at probe time.
func (b *containerBackend) resolveUsernsMode(ctx context.Context) container.UsernsMode {
	b.usernsOnce.Do(func() {
		version, versionErr := b.api.ServerVersion(ctx, client.ServerVersionOptions{})
		if versionErr != nil {
			slog.Warn("container backend: engine version probe failed; job containers will use the default userns mode",
				"error", versionErr)
		}
		info := b.resolveEngineInfo(ctx)
		b.usernsMode = usernsModeForEngine(version, info)
		if b.usernsMode != "" {
			slog.Info("container backend: rootless podman detected; job containers will keep the host uid mapping",
				"userns_mode", string(b.usernsMode))
		}
	})
	return b.usernsMode
}

// resolveEngineInfo probes the engine's /info, caching only a successful
// result — shared by resolveUsernsMode (rootless detection) and
// resolveHostArch (arch mismatch fail-fast) below, so both reuse one round
// trip once it succeeds (TestContainerBackend_Launch_EngineProbeOncePerBackend
// pins this).
//
// A failed probe is logged and deliberately not cached: caching a failure
// like a success would silently and permanently disable resolveHostArch's
// arch mismatch fail-fast after one transient /info hiccup. Every call
// after a failure retries fresh until one succeeds.
func (b *containerBackend) resolveEngineInfo(ctx context.Context) client.SystemInfoResult {
	b.infoMu.Lock()
	defer b.infoMu.Unlock()
	if b.infoOK {
		return b.info
	}
	info, err := b.api.Info(ctx, client.InfoOptions{})
	if err != nil {
		slog.Warn("container backend: engine info probe failed", "error", err)
		return client.SystemInfoResult{}
	}
	b.info = info
	b.infoOK = true
	return b.info
}

// normalizeArch maps an engine's own uname(1)-style /info architecture
// string ("x86_64"/"aarch64") onto the Go/OCI vocabulary an image
// manifest's Architecture field uses ("amd64"/"arm64") — resolveImage's
// arch mismatch check compares one against the other, so without this a
// native amd64 host would falsely mismatch its own images. Unrecognized
// input passes through lowercased unchanged: resolveImage only acts on a
// positive, known mismatch, so an unmapped string just no-ops.
func normalizeArch(arch string) string {
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	case "armv7l", "armv6l", "arm":
		return "arm"
	case "i386", "i686", "386":
		return "386"
	default:
		return strings.ToLower(strings.TrimSpace(arch))
	}
}

// NormalizeArch is normalizeArch, exported for cmd/check.go: `boid check`
// runs its own diagnostic host-arch vs. image-arch preflight and needs the
// exact same translation resolveImage's launch-time check applies, or the
// two would disagree about what counts as a mismatch. Kept as a thin
// wrapper so call sites inside this package keep using the private name.
func NormalizeArch(arch string) string {
	return normalizeArch(arch)
}

// resolveHostArch returns the engine's own reported host architecture,
// normalized to the Go/OCI vocabulary (normalizeArch), reusing
// resolveEngineInfo's shared cached probe. Deliberately not runtime.GOARCH:
// the running Go binary's build architecture is meaningless as a "real
// machine" signal once that binary might itself be running inside an
// emulated container, while dockerd/podman always run natively on the real
// host.
//
// Returns "" when the engine reported no architecture, including a failed
// probe (retried on the next call, not cached — see resolveEngineInfo).
// resolveImage's mismatch check only fires when both sides are non-empty,
// so "" degrades that call to "arch check skipped" rather than blocking
// launch on a transient failure.
func (b *containerBackend) resolveHostArch(ctx context.Context) string {
	info := b.resolveEngineInfo(ctx)
	if info.Info.Architecture == "" {
		return ""
	}
	return normalizeArch(info.Info.Architecture)
}
