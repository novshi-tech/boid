package dispatcher

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// This file holds the guards for PR4 of
// docs/plans/workspace-home-volume-persistence.md: the workspace slug is
// THREADED from the one place that normalizes it (resolveWorkspaceHome, via
// normalizeWorkspaceSlug) to the one place that publishes it
// (SandboxRuntimeInfo.WorkspaceSlug -> env BOID_WORKSPACE_SLUG -> the
// claude/codex/opencode adapters' "CLI not found in workspace $HOME" error),
// instead of being re-derived from the resolved home directory's path.
//
// Why this needs its own file: Dispatch used to fill the field with
// filepath.Base(workspaceHomeDir), which was CORRECT only while a home
// directory was named after its slug. PR6 replaced that home with a named
// volume (boid-ws-home-<installID8>-<slug>, 論点a), so the basename is no
// longer the slug and a re-derivation would make both the env var and the
// adapter error message name a workspace that does not exist — the operator
// would be told to edit
// ~/.config/boid/workspaces/boid-ws-home-1a2b3c4d-myws/init.sh.
//
// PR4 had to SIMULATE that divergence with a swappable home-name function,
// because the two values were equal in the production layout of the day and
// every assertion here would otherwise have been tautological. PR6 made the
// divergence real, so the stub is gone and each case asserts the divergence
// directly against the value production actually produces.

// TestDispatch_WorkspaceSlug_ReachesLaunchedSpecEnvWithoutPathDerivation is
// the wiring-seam guard for PR4, asserted against the sandbox.Spec the
// BACKEND actually received — the same shape as PR2's
// TestWire_DataHomeDir_ReachesMarkerAndLockOnDisk and PR3's
// TestDispatch_EmbeddedSkills_ReachLaunchedSpecAsPerSkillReadOnlyBinds, and
// for the same reason: neither a unit test of resolveWorkspaceHome nor
// TestBuildSandboxSpec_WorkspaceSlugEnv (which hand-builds the
// SandboxRuntimeInfo) can see Dispatch put the WRONG value into the field
// they each half-cover.
//
// The case pins the whole chain in one dispatch:
//
//	normalizeWorkspaceSlug -> resolveWorkspaceHome's returned slug
//	  -> SandboxRuntimeInfo.WorkspaceSlug -> BuildSandboxSpec
//	  -> spec.Env["BOID_WORKSPACE_SLUG"] (= adapters' RunContext.Env)
//
// and it is run under a home layout whose basename is not the slug, so
// re-deriving the value with filepath.Base at any link fails here.
func TestDispatch_WorkspaceSlug_ReachesLaunchedSpecEnvWithoutPathDerivation(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	runtimesDir := filepath.Join(t.TempDir(), "runtimes")

	r := skillsSyncTestRunner(t, runtimesDir, "myws")
	be, ok := r.Backend.(*gwFakeBackend)
	if !ok {
		t.Fatalf("test wiring: Runner.Backend is %T, want *gwFakeBackend (the stub that records launched specs)", r.Backend)
	}

	if _, err := r.Dispatch(context.Background(), skillsSyncTestSpec(), nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(be.launched) != 1 {
		t.Fatalf("backend received %d Launch calls, want exactly 1", len(be.launched))
	}
	launched := be.launched[0]

	// Vacuity guard: prove the divergence this whole case rests on is real
	// before asserting anything about it. The workspace home actually mounted
	// at the sandbox's $HOME must be the stubbed, non-slug-named directory —
	// if the stub silently stopped taking effect, every assertion below would
	// pass against a filepath.Base implementation too.
	homeIdx := mountTargetIndex(launched.Mounts, hostHomeDir())
	if homeIdx == -1 {
		t.Fatalf("the launched sandbox.Spec has no mount at the sandbox $HOME (%s); the workspace home was not mounted, so this case cannot prove anything: mounts=%+v",
			hostHomeDir(), launched.Mounts)
	}
	homeSource := launched.Mounts[homeIdx].Source
	if homeSource == "myws" || filepath.Base(homeSource) == "myws" {
		t.Fatalf("test wiring: the mounted workspace home (%s) is named after its slug, so this case would be tautological — PR6's volume name is boid-ws-home-<installID8>-<slug>", homeSource)
	}

	if got := launched.Env["BOID_WORKSPACE_SLUG"]; got != "myws" {
		t.Errorf("BOID_WORKSPACE_SLUG in the launched spec = %q, want %q — Dispatch is deriving the workspace slug from the resolved home directory (%s) instead of threading the slug resolveWorkspaceHome normalized",
			got, "myws", homeSource)
	}
}

// TestDispatch_WorkspaceSlug_DefaultWorkspace_ReachesLaunchedSpecEnv covers
// the branch where the slug is not the project's WorkspaceID at all:
// normalizeWorkspaceSlug maps an unassigned project ("" WorkspaceID) onto
// orchestrator.DefaultWorkspaceSlug. That value has no other source in
// Dispatch — workspaceID is still "" there — so this is the case where
// threading is not merely tidier than re-deriving but the only way to get the
// right answer once the path stops carrying the slug.
func TestDispatch_WorkspaceSlug_DefaultWorkspace_ReachesLaunchedSpecEnv(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	runtimesDir := filepath.Join(t.TempDir(), "runtimes")

	r := skillsSyncTestRunner(t, runtimesDir, "")
	be, ok := r.Backend.(*gwFakeBackend)
	if !ok {
		t.Fatalf("test wiring: Runner.Backend is %T, want *gwFakeBackend", r.Backend)
	}

	if _, err := r.Dispatch(context.Background(), skillsSyncTestSpec(), nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(be.launched) != 1 {
		t.Fatalf("backend received %d Launch calls, want exactly 1", len(be.launched))
	}

	if got := be.launched[0].Env["BOID_WORKSPACE_SLUG"]; got != orchestrator.DefaultWorkspaceSlug {
		t.Errorf("BOID_WORKSPACE_SLUG in the launched spec = %q, want %q (orchestrator.DefaultWorkspaceSlug, what normalizeWorkspaceSlug maps an unassigned project to)", got, orchestrator.DefaultWorkspaceSlug)
	}
}

// TestDispatch_WorkspaceHomeIdentity_ReachesLaunchOptions is the same kind of
// seam guard for the identity PR6's codex review (Major 1) added: the value
// resolveWorkspaceHome observed on the home volume has to reach the BACKEND, or
// the backend's own re-check of it is inert.
//
// It is the classic "both ends wired, nothing crosses" shape, and unusually
// invisible here: containerBackend treats an EMPTY WorkspaceHomeID as "this
// caller did not resolve a home" and skips the comparison entirely (DI/test
// wiring calls Launch directly). So a Dispatch that stopped filling the field
// would not fail anything — it would silently go back to mounting whatever
// volume happens to hold the name, which is exactly the state Major 1
// described.
func TestDispatch_WorkspaceHomeIdentity_ReachesLaunchOptions(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	runtimesDir := filepath.Join(t.TempDir(), "runtimes")

	r := skillsSyncTestRunner(t, runtimesDir, "myws")
	be, ok := r.Backend.(*gwFakeBackend)
	if !ok {
		t.Fatalf("test wiring: Runner.Backend is %T, want *gwFakeBackend", r.Backend)
	}

	if _, err := r.Dispatch(context.Background(), skillsSyncTestSpec(), nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(be.launchOpts) != 1 {
		t.Fatalf("backend received %d Launch calls, want exactly 1", len(be.launchOpts))
	}
	opts := be.launchOpts[0]

	want := be.workspaceHomes().identityOf(t, dockerres.WorkspaceHomeVolumeName(r.InstallID, "myws"))
	if want == "" {
		t.Fatal("test wiring: the modelled home volume carries no identity, so this case cannot prove anything")
	}
	if opts.WorkspaceHomeID != want {
		t.Errorf("LaunchOptions.WorkspaceHomeID = %q, want the identity resolveWorkspaceHome observed (%q). "+
			"An empty value disables containerBackend's own check silently — see verifyWorkspaceHomeIdentity",
			opts.WorkspaceHomeID, want)
	}
	if opts.WorkspaceSlug != "myws" {
		t.Errorf("LaunchOptions.WorkspaceSlug = %q, want %q", opts.WorkspaceSlug, "myws")
	}
}

// TestResolveWorkspaceHome_ReturnsNormalizedSlugAlongsideHomeDir pins the
// producing end on its own: resolveWorkspaceHome hands back the slug it
// normalized, so no caller has to reconstruct it. A "return
// filepath.Base(<first return value>)" implementation fails here rather than
// passing by coincidence, because the first return value is a volume name.
func TestResolveWorkspaceHome_ReturnsNormalizedSlugAlongsideHomeDir(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)

	for _, tc := range []struct {
		name        string
		workspaceID string
		wantSlug    string
	}{
		{"explicit workspace", "myws", "myws"},
		{"unassigned project normalizes to the default workspace", "", orchestrator.DefaultWorkspaceSlug},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{RuntimesDir: filepath.Join(t.TempDir(), "runtimes"), Backend: newBashWorkspaceInitBackend(t)}

			home, slug, _, err := r.resolveWorkspaceHome(context.Background(), tc.workspaceID)
			if err != nil {
				t.Fatalf("resolveWorkspaceHome(%q): %v", tc.workspaceID, err)
			}
			if home == tc.wantSlug || filepath.Base(home) == tc.wantSlug {
				t.Fatalf("test wiring: the resolved home %q is named after its slug, so this case would be tautological", home)
			}
			if slug != tc.wantSlug {
				t.Errorf("resolveWorkspaceHome(%q) slug = %q, want %q (the normalized slug, not anything derived from the home %q)",
					tc.workspaceID, slug, tc.wantSlug, home)
			}
			if want := dockerres.WorkspaceHomeVolumeName(r.InstallID, tc.wantSlug); home != want {
				t.Errorf("resolveWorkspaceHome(%q) home = %q, want the named volume %q", tc.workspaceID, home, want)
			}
		})
	}
}
