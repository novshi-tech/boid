package dispatcher

// container_backend_userns.go resolves the userns mode job containers are
// created with. See container_backend_userns_test.go's own header comment
// for the full failure this exists to close (2026-07-26 volume-only
// dogfood: rootless podman maps a job container's uid 1000 to a host
// subuid, so every daemon-created bind mount — most consequentially the
// 0700 per-workspace HOME — became unreadable to the job itself).

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
		info, infoErr := b.api.Info(ctx, client.InfoOptions{})
		if infoErr != nil {
			slog.Warn("container backend: engine info probe failed; job containers will use the default userns mode",
				"error", infoErr)
		}
		b.usernsMode = usernsModeForEngine(version, info)
		if b.usernsMode != "" {
			slog.Info("container backend: rootless podman detected; job containers will keep the host uid mapping",
				"userns_mode", string(b.usernsMode))
		}
	})
	return b.usernsMode
}
