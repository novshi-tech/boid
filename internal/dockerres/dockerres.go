// Package dockerres is the single source of truth for the names and labels
// boid puts on the docker resources it creates, shared by internal/reap,
// internal/dispatcher, and internal/sandbox/dockerproxy so their naming and
// containment rules cannot drift apart (see
// docs/plans/workspace-home-volume-persistence.md 論点 a).
//
// This package MUST stay a leaf: no boid imports at all, and nothing from
// the standard library beyond basic string handling.
//
// # The two volume-name prefixes are not interchangeable
//
// ReservedVolumeNamePrefix ("boid-ws-") is the namespace boid owns. It is
// deliberately WIDER than the set of volumes that must survive: it is also
// the prefix of the per-workspace docker NETWORK names WorkspaceNetworkName
// produces. It must never be applied to networks: boid creates those
// itself and the sandbox is not the one naming them.
//
// WorkspaceHomeVolumePrefix ("boid-ws-home-") is the NARROWER set the
// reapers skip. Only these volumes hold state that cannot be regenerated
// (harness credentials, a ~1.5GB toolchain) — a workspace network is
// recreated on demand, so reap is still free to destroy it.
package dockerres

import "strings"

// Label keys boid puts on the docker resources it creates.
//
// LabelJobID / LabelWorkspace / LabelInstallID are the job resource labels:
// boid.job_id + boid.workspace on every job container, boid.install_id
// whenever the install has an id. Both reapers enumerate on them —
// containerBackend.ReapOrphans filters on the presence of boid.job_id,
// reap.Run on boid.install_id=<id>.
//
// LabelWorkspaceHome / LabelWorkspaceHomeInstallID are the workspace HOME
// volume's own labels, kept separate from the job labels above precisely
// because those are enumeration filters a persistent home volume must not
// match (docs/plans/workspace-home-volume-persistence.md 論点 a): matching
// boid.install_id or boid.job_id would get it force-removed by one of the
// reapers' sweeps.
const (
	LabelJobID     = "boid.job_id"
	LabelWorkspace = "boid.workspace"
	LabelInstallID = "boid.install_id"

	// LabelWorkspaceHome carries the workspace slug. Its mere presence is
	// also what containerBackend.reapOrphanVolumes uses as a second,
	// name-independent signal that a volume must not be destroyed.
	LabelWorkspaceHome = "boid.workspace_home"

	// LabelWorkspaceHomeInstallID carries the install id — the scoping
	// boid.install_id would normally provide, under a key no reap filter
	// enumerates on.
	LabelWorkspaceHomeInstallID = "boid.workspace_home_install_id"

	// LabelWorkspaceHomeID carries a per-volume identity token: 32 bytes of
	// crypto/rand, hex-encoded, minted when the volume is created and never
	// changed afterwards. It lets the daemon distinguish "the home my
	// completion marker describes" from "a different home that happens to
	// have the same name" — the name alone is a pure function of (install
	// id, slug), so a volume deleted and re-created comes back under the
	// same name, empty.
	//
	// This is a LABEL rather than a file inside the volume because
	// VolumeCreate is idempotent and returns an existing volume's labels
	// for a name that already exists, so "ensure the volume exists" and
	// "read its identity" collapse into one call. See
	// internal/dispatcher/workspace_home.go's workspaceHomeMarker.HomeID
	// for how this is consumed.
	LabelWorkspaceHomeID = "boid.workspace_home_id"

	// LabelWorkspaceInit carries the workspace slug of a throwaway workspace
	// HOME init container. It has its own key, distinct from boid.job_id and
	// boid.install_id, so an init container mid-toolchain-download is
	// invisible to both reapers' sweeps — see
	// containerBackend.reapOrphanWorkspaceInitContainers for the dedicated
	// startup sweep that still collects one orphaned by a daemon crash.
	LabelWorkspaceInit = "boid.workspace_init"

	// LabelWorkspaceInitInstallID carries the install id for the init
	// container, under a key no other sweep enumerates on — same arrangement
	// as LabelWorkspaceHomeInstallID.
	LabelWorkspaceInitInstallID = "boid.workspace_init_install_id"
)

// Volume/network name prefixes. See the package doc for why these two are
// distinct and must not be substituted for one another.
const (
	// ReservedVolumeNamePrefix is the workspace-scoped namespace boid owns.
	ReservedVolumeNamePrefix = "boid-ws-"

	// WorkspaceHomeVolumePrefix is the persistent workspace HOME volume
	// namespace — the volumes both reapers skip by default.
	WorkspaceHomeVolumePrefix = ReservedVolumeNamePrefix + "home-"

	// WorkspaceInitContainerPrefix namespaces the throwaway CONTAINER that
	// prepares a workspace HOME. Note this is a container namespace, not a
	// volume one: it shares ReservedVolumeNamePrefix's leading text purely
	// so `docker ps`/`docker volume ls` output reads consistently, and
	// IsReservedVolumeName must never be pointed at it — a sandboxed docker
	// client naming its own container is not the threat that predicate is
	// about.
	WorkspaceInitContainerPrefix = ReservedVolumeNamePrefix + "init-"

	// noInstallIDPart substitutes for an empty install id, so a name stays
	// deterministic (just not install-scoped) for the test/DI callers that
	// never set one.
	noInstallIDPart = "noinst"

	// installIDNameLen is how much of the install id goes into a resource
	// name — enough to disambiguate two installs sharing one docker engine
	// without making names unreadable.
	installIDNameLen = 8
)

// IsReservedVolumeName reports whether name is inside the volume namespace
// boid owns and a sandboxed docker client therefore may not create or
// delete. Callers that want "must this volume survive a reap?" want
// IsWorkspaceHomeVolumeName instead — this predicate is deliberately wider.
func IsReservedVolumeName(name string) bool {
	return strings.HasPrefix(name, ReservedVolumeNamePrefix)
}

// IsWorkspaceHomeVolumeName reports whether name is a persistent workspace
// HOME volume, i.e. one that holds state boid cannot regenerate and that
// both reapers must leave alone unless explicitly told otherwise
// (internal/reap.IncludeWorkspaceHomes).
func IsWorkspaceHomeVolumeName(name string) bool {
	return strings.HasPrefix(name, WorkspaceHomeVolumePrefix)
}

// SanitizeNamePart maps every rune outside docker's `[a-zA-Z0-9_.-]` name-
// body charset to '-', and substitutes a placeholder for an empty result,
// so an unexpected workspace slug or install id can never produce an
// invalid docker name. It only sanitizes the BODY: the leading-character
// rule is satisfied by the callers below always prefixing "boid-ws-".
func SanitizeNamePart(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}

// WorkspaceNetworkName returns the deterministic docker network name a
// Workspace-scoped Launch and the runner's matching dockerproxy
// SetWorkspaceNetwork call both compute independently for the SAME
// (installID, workspace) pair. It must be a pure function of just those two
// values — not cached state on either side — because the two call sites
// construct their own pieces of a job's sandbox independently and must
// still land on the same network without coordinating directly.
//
// installID scopes the name so two independent boid installations sharing
// one docker engine never collide.
func WorkspaceNetworkName(installID, workspace string) string {
	return ReservedVolumeNamePrefix + installIDPart(installID) + "-" + SanitizeNamePart(workspace)
}

// WorkspaceHomeVolumeName returns the deterministic name of a workspace's
// persistent HOME volume, following WorkspaceNetworkName's convention.
// dispatcher.resolveWorkspaceHome returns this name, and both the job
// container and the init container mount it at the harness's $HOME.
//
// Determinism makes the name recomputable from (install id, slug) alone, so
// nothing has to persist the mapping. The volume's IDENTITY — which
// incarnation of that name a completion marker describes — is a separate
// matter and lives in LabelWorkspaceHomeID, precisely because the name
// cannot express it.
func WorkspaceHomeVolumeName(installID, workspace string) string {
	return WorkspaceHomeVolumePrefix + installIDPart(installID) + "-" + SanitizeNamePart(workspace)
}

// WorkspaceInitContainerName returns the deterministic name of the throwaway
// container that runs a workspace HOME's prep + init.sh, following
// WorkspaceNetworkName's convention.
//
// Determinism is the mechanism, not a convenience: after a daemon crash
// mid-init, the next dispatch computes the SAME name, so the engine itself
// rejects a second create with a 409 rather than racing the incumbent init.
func WorkspaceInitContainerName(installID, workspace string) string {
	return WorkspaceInitContainerPrefix + installIDPart(installID) + "-" + SanitizeNamePart(workspace)
}

// installIDPart renders the install-id segment shared by the name
// builders above.
func installIDPart(installID string) string {
	if installID == "" {
		return noInstallIDPart
	}
	part := SanitizeNamePart(installID)
	if len(part) > installIDNameLen {
		part = part[:installIDNameLen]
	}
	return part
}

// IsValidVolumeName reports whether name matches docker's own volume-name
// grammar, `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`. Rejecting an invalid name turns a
// confusing engine-side error (or a silently auto-created junk volume) into
// a fail-closed Launch error naming the offending source.
func IsValidVolumeName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			// Always allowed, including as the first character.
		case r == '_' || r == '.' || r == '-':
			if i == 0 {
				return false // grammar's first character class excludes these
			}
		default:
			return false
		}
	}
	return true
}
