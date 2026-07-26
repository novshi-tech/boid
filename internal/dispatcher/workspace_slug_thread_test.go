package dispatcher

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

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
// Why this needs its own file and its own seam: Dispatch used to fill the
// field with filepath.Base(workspaceHomeDir), which is CORRECT today only
// because workspaceHomeDirFor names a home directory after its slug. PR6
// replaces that name with a named volume (boid-ws-home-<installID8>-<slug>,
// 論点a), at which point the basename stops being the slug and both the env
// var and the adapter error message silently start naming a workspace that
// does not exist — the operator is then told to edit
// ~/.config/boid/workspaces/boid-ws-home-1a2b3c4d-myws/init.sh.
//
// Every assertion below therefore runs against a home directory whose
// basename is deliberately NOT the slug (stubWorkspaceHomeDirName), and each
// case asserts that divergence itself first. Without that, the assertions
// would be tautological: the two values are equal in today's production
// layout, so a test that merely checks "the slug is myws" passes just as well
// against the filepath.Base implementation it is meant to rule out.

// stubWorkspaceHomeDirPrefix is the marker the stubbed layout prepends to a
// slug to build the home directory's basename. Shaped after the volume name
// PR6 introduces (docs/plans/workspace-home-volume-persistence.md 論点a) so
// the divergence being simulated is the one that actually arrives, not an
// arbitrary one.
const stubWorkspaceHomeDirPrefix = "boid-ws-home-1a2b3c4d-"

// stubWorkspaceHomeDirName makes resolveWorkspaceHome place a workspace's
// home at <homesDir>/boid-ws-home-1a2b3c4d-<slug> for the duration of the
// calling test, restoring the production layout afterwards.
//
// The stub still derives the directory from the slug (rather than returning
// some fixed constant) so that distinct slugs keep distinct homes and the
// rest of resolveWorkspaceHome — marker, lock, nonce, init.sh — behaves
// exactly as in production. What it removes is only the accidental identity
// filepath.Base(homeDir) == slug.
func stubWorkspaceHomeDirName(t *testing.T) {
	t.Helper()
	orig := workspaceHomeDirFor
	t.Cleanup(func() { workspaceHomeDirFor = orig })
	workspaceHomeDirFor = func(homesDir, slug string) string {
		return filepath.Join(homesDir, stubWorkspaceHomeDirPrefix+slug)
	}
}

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
	stubWorkspaceHomeDirName(t)
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
	if base := filepath.Base(homeSource); base == "myws" {
		t.Fatalf("test wiring: the workspace home directory is still named after its slug (%s); stubWorkspaceHomeDirName did not take effect and this case would be tautological", homeSource)
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
	stubWorkspaceHomeDirName(t)
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

// TestResolveWorkspaceHome_ReturnsNormalizedSlugAlongsideHomeDir pins the
// producing end on its own: resolveWorkspaceHome hands back the slug it
// normalized, so no caller has to reconstruct it. Same non-slug-named layout,
// so a "return filepath.Base(homeDir)" implementation of the second return
// value fails here rather than passing by coincidence.
func TestResolveWorkspaceHome_ReturnsNormalizedSlugAlongsideHomeDir(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	stubWorkspaceHomeDirName(t)

	for _, tc := range []struct {
		name        string
		workspaceID string
		wantSlug    string
	}{
		{"explicit workspace", "myws", "myws"},
		{"unassigned project normalizes to the default workspace", "", orchestrator.DefaultWorkspaceSlug},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{RuntimesDir: filepath.Join(t.TempDir(), "runtimes")}

			homeDir, slug, err := r.resolveWorkspaceHome(tc.workspaceID)
			if err != nil {
				t.Fatalf("resolveWorkspaceHome(%q): %v", tc.workspaceID, err)
			}
			if base := filepath.Base(homeDir); base == tc.wantSlug {
				t.Fatalf("test wiring: home dir %q is still named after its slug; stubWorkspaceHomeDirName did not take effect and this case would be tautological", homeDir)
			}
			if slug != tc.wantSlug {
				t.Errorf("resolveWorkspaceHome(%q) slug = %q, want %q (the normalized slug, not anything derived from home dir %q)",
					tc.workspaceID, slug, tc.wantSlug, homeDir)
			}
			if !strings.HasSuffix(homeDir, stubWorkspaceHomeDirPrefix+tc.wantSlug) {
				t.Errorf("resolveWorkspaceHome(%q) home dir = %q, want it to end in %q", tc.workspaceID, homeDir, stubWorkspaceHomeDirPrefix+tc.wantSlug)
			}
		})
	}
}
