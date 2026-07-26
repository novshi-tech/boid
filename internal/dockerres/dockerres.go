// Package dockerres is the single source of truth for the names and labels
// boid puts on the docker resources it creates.
//
// It exists because the three packages that have to agree on those strings
// cannot import each other. internal/reap must stay importable from cmd
// without pulling in internal/dispatcher's dependency graph (DB,
// orchestrator, sqlite — see internal/reap's own package doc: `boid reap`
// has to work "even when the daemon is unable to start"), and
// internal/sandbox/dockerproxy sits below both. The pre-existing answer was
// to hand-copy each literal into every package and warn about drift in a doc
// comment; docs/plans/workspace-home-volume-persistence.md 論点 a turns that
// drift into data loss — a containment rule that guards
// "boid-ws-home-" in one package and a name generator that emits
// "boid-ws-home" in another silently destroys a workspace's credentials —
// so the strings moved here instead.
//
// The dependency direction dispatcher → reap → dockerproxy is unchanged;
// all three (and cmd) may import this package. To keep that true this
// package MUST stay a leaf: no boid imports at all, and nothing from the
// standard library beyond basic string handling.
//
// # The two volume-name prefixes are not interchangeable
//
// ReservedVolumeNamePrefix ("boid-ws-") is the namespace boid owns. It is
// deliberately WIDER than the set of volumes that must survive: it is also
// the prefix of the per-workspace docker NETWORK names WorkspaceNetworkName
// produces. dockerproxy's policy denies a sandboxed docker client creating
// or deleting a VOLUME in this namespace (a job could otherwise name its
// scratch volume exactly like boid's and have boid's own per-job reaper
// delete the real one — docs/plans/workspace-home-volume-persistence.md
// 論点 a 経路 4). It must never be applied to networks: boid creates those
// itself and the sandbox is not the one naming them.
//
// WorkspaceHomeVolumePrefix ("boid-ws-home-") is the NARROWER set the
// reapers skip. Only these volumes hold state that cannot be regenerated
// (harness credentials, a ~1.5GB toolchain). A workspace network is
// recreated on demand by ensureWorkspaceNetwork, so reap is still free to
// destroy it — which is why the skip rules key off this prefix and not the
// reserved one.
package dockerres

import "strings"

// Label keys boid puts on the docker resources it creates.
//
// LabelJobID / LabelWorkspace / LabelInstallID are the pre-existing job
// resource labels (docs/plans/phase6-container-backend.md §決定 6/9):
// boid.job_id + boid.workspace on every job container,
// boid.install_id whenever the install has an id. Both reapers enumerate on
// them — containerBackend.ReapOrphans filters on the presence of
// boid.job_id, reap.Run on boid.install_id=<id>.
//
// LabelWorkspaceHome / LabelWorkspaceHomeInstallID are the workspace HOME
// volume's own labels, and the reason they are separate keys rather than
// reuses of the ones above is precisely that the ones above are enumeration
// filters: a persistent volume carrying boid.install_id would be listed —
// and unconditionally force-removed — by reap.Run on every daemon startup
// (docs/plans/workspace-home-volume-persistence.md 論点 a 経路 1), and one
// carrying boid.job_id would be listed by reapOrphanVolumes (経路 2). The
// install scope still has to be recorded somewhere, so it gets its own key
// that no existing filter looks at.
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

	// LabelWorkspaceInit carries the workspace slug of a throwaway workspace
	// HOME init container (docs/plans/workspace-home-volume-persistence.md
	// 論点 c, PR5). Its own key for the same reason LabelWorkspaceHome has
	// one, but the consequence of getting it wrong is different in kind:
	//
	//   - boid.job_id would put the container in containerBackend.ReapOrphans'
	//     primary sweep, which lists on the mere PRESENCE of that key and
	//     removes with Force+RemoveVolumes. An init container is routinely
	//     minutes into a ~1.5GB toolchain download, so a daemon restart would
	//     kill it mid-install.
	//   - worse, the job id read off that label would then land in
	//     ReapReport.ReapedJobIDs / FailedJobIDs, which drive MarkStale and
	//     gate auto-reopen — so a workspace init would corrupt the job
	//     accounting of unrelated tasks (a remove failure alone suppresses
	//     another task's auto-reopen for that whole boot).
	//   - boid.install_id would put it in internal/reap.Run's own sweep.
	//
	// Being invisible to those sweeps is not the same as being nobody's
	// problem: containerBackend.reapOrphanWorkspaceInitContainers enumerates
	// on THIS key at startup so a container orphaned by a daemon crash is
	// still collected, without touching the job accounting above.
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
	// prepares a workspace HOME (PR5). Note this is a container namespace, not
	// a volume one: it shares ReservedVolumeNamePrefix's leading text purely
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
// (installID, workspace) pair (docs/plans/phase6-container-backend.md §決定5,
// "internal network は workspace 単位で分離する"). It has to be a pure
// function of just those two values — not cached state on either side —
// because the two call sites construct their own pieces of a job's sandbox
// independently and must still land on the same network without
// coordinating directly.
//
// installID scopes the name so two independent boid installations sharing
// one docker engine never collide.
func WorkspaceNetworkName(installID, workspace string) string {
	return ReservedVolumeNamePrefix + installIDPart(installID) + "-" + SanitizeNamePart(workspace)
}

// WorkspaceHomeVolumeName returns the deterministic name of a workspace's
// persistent HOME volume, following WorkspaceNetworkName's convention.
//
// Nothing calls this yet: the volume is created and mounted by PR6 of
// docs/plans/workspace-home-volume-persistence.md. It lives here in PR1
// anyway so the name and the WorkspaceHomeVolumePrefix that PR1's
// containment rules key off can never drift apart — a generator emitting a
// name the skip rules do not recognize is exactly the failure this whole
// package exists to prevent (see the package doc).
func WorkspaceHomeVolumeName(installID, workspace string) string {
	return WorkspaceHomeVolumePrefix + installIDPart(installID) + "-" + SanitizeNamePart(workspace)
}

// WorkspaceInitContainerName returns the deterministic name of the throwaway
// container that runs a workspace HOME's prep + init.sh
// (docs/plans/workspace-home-volume-persistence.md 論点 c, PR5), following
// WorkspaceNetworkName's convention.
//
// Determinism is the mechanism, not a convenience. The flock that serializes
// init runs (dispatcher.acquireWorkspaceHomeLock) is released when its process
// dies, but a container outlives the process that created it — so after a
// daemon crash the next dispatch would start a second init while the first one
// is still writing into the same home, breaking Phase 4's "同時 dispatch でも
// 実行は 1 回" contract (docs/plans/home-workspace-volume.md:83). Two runs
// computing the same name means the engine itself rejects the second create
// with a 409, which the caller resolves by waiting for (or removing) the
// incumbent instead of racing it.
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
// grammar, `^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`.
//
// This is the guard docs/plans/workspace-home-volume-persistence.md 論点 e
// requires for its option (i): the container backend classifies a mount
// source as a named volume purely by "it does not start with /"
// (internal/sandbox/realization.classifySource), so a relative path that
// reaches the mount list by accident would otherwise be handed to
// VolumeCreate as if it were a volume name. Rejecting it turns a confusing
// engine-side error (or, worse, a silently auto-created junk volume) into a
// fail-closed Launch error naming the offending source.
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
