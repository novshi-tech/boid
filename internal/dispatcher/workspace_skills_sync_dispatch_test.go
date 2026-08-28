package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/skills"
)

// This file holds the Dispatch-level wiring guards for how embedded skills
// reach a job: proof that a dispatch leaves a symlink for every embedded skill
// in every discovery root inside the workspace home, and that the skeleton
// those roots need reaches the launched spec.
//
// It has been inverted twice, which is worth knowing before adding to it.
// Phase 4 PR3 (docs/plans/home-workspace-volume.md) copy-synced the skill
// CONTENT into the workspace home, and this file asserted
// <home>/.claude/skills/boid-task/SKILL.md existed after a dispatch. PR3 of
// docs/plans/workspace-home-volume-persistence.md (論点 e-2) moved the write
// out to a host-visible runtimes directory and bind-mounted it back in, so the
// assertion inverted: the home must NOT contain the content, and the launched
// spec must carry one read-only bind per skill. Baking the set into the runner
// image inverts it again — there is no daemon-side materialization left to
// assert, no bind to find on the spec, and the home holds a symlink per skill
// per root.
//
// What survived all three: a dispatch has to leave the workspace home in a
// state where the harness can find /boid-task. Only the mechanism moved.
//
// Test helpers (setupWorkspaceHomeTestDirs, fakeProjectLookup,
// gwFakeSandboxPrep, gwFakeRuntime, newGatewayTestDB) are shared with
// workspace_home_test.go / workspace_home_dispatch_test.go /
// gitgateway_wire_test.go — all in this same package.

// skillsSyncTestRunner builds the minimal Runner + project + DB wiring these
// cases share, rooted at a caller-supplied runtimes dir so each case can
// point RuntimesDir at its own tree (homes/ is derived from it — see
// WorkspaceHomesDir).
func skillsSyncTestRunner(t *testing.T, runtimesDir, workspaceID string) *Runner {
	t.Helper()
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	return &Runner{
		DB: d.Conn,
		Projects: fakeProjectLookup{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkDir: "/tmp", WorkspaceID: workspaceID},
		}},
		Backend:     &gwFakeBackend{},
		BoidBinary:  "/boid",
		RuntimesDir: runtimesDir,
	}
}

func skillsSyncTestSpec() *orchestrator.JobSpec {
	return &orchestrator.JobSpec{
		ProjectID: "proj-1",
		Argv:      []string{"echo", "hi"},
		Kind:      orchestrator.JobKindHook,
	}
}

// TestDispatch_EmbeddedSkills_ReachTheWorkspaceHomeAsSymlinks is the crossing
// guard for the whole mechanism, driven end to end: a real Dispatch, through
// the real resolveWorkspaceHome, through a backend that actually runs the
// generated wrapper under bash, asserting against the resulting home.
//
// Neither end's own tests can state this claim. skill_links_test.go proves the
// wrapper writes both roots when handed a SkillLink;
// TestSkillLinks_IncludesEmbeddedSkillsPointingAtTheImageDir proves the
// embedded set turns into SkillLinks. Both would stay green if
// resolveWorkspaceHome stopped passing skillLinks(r.Packs) into the request —
// which is exactly the shape of this repo's recurring "both ends wired,
// nothing crosses" failure, and the reason the per-skill-bind version of this
// test existed before it.
//
// The symlinks DANGLE here, and that is not a defect of the test: Target is
// embeddedSkillsImageDir, a path inside the runner image, which the machine
// running `go test` has no reason to have. `ln -sfn` does not require its
// target to exist, so what a dispatch is responsible for — the link, in the
// right place, pointing at the right path — is fully observable without one.
func TestDispatch_EmbeddedSkills_ReachTheWorkspaceHomeAsSymlinks(t *testing.T) {
	embedded := skills.EmbeddedSkillNames()
	if len(embedded) == 0 {
		t.Skip("no embedded skills compiled into this binary")
	}
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

	homeDir := be.workspaceHomeDir(dockerres.WorkspaceHomeVolumeName(r.InstallID, "myws"))
	for _, name := range embedded {
		for _, root := range skillDiscoveryRoots {
			rel := filepath.Join(root, name)
			linkPath := filepath.Join(homeDir, rel)
			info, err := os.Lstat(linkPath)
			if err != nil {
				t.Errorf("after Dispatch the workspace home has no %s: %v", rel, err)
				continue
			}
			if info.Mode()&os.ModeSymlink == 0 {
				t.Errorf("%s is not a symlink (mode %s)", rel, info.Mode())
				continue
			}
			target, err := os.Readlink(linkPath)
			if err != nil {
				t.Errorf("readlink %s: %v", rel, err)
				continue
			}
			if want := filepath.Join(embeddedSkillsImageDir, name); target != want {
				t.Errorf("%s -> %q, want the image-baked %q", rel, target, want)
			}
		}
	}
}

// TestDispatch_EmbeddedSkills_LeaveNoPerSkillBindOnTheLaunchedSpec is the
// other half, and it is not redundant with TestHomeMounts_DeclaresNoPerSkillBinds:
// that one pins the function, this one pins that nothing downstream of
// Dispatch puts the mounts back by another route. A surviving bind would name
// a source under the runtimes root that nothing materializes any more, and
// those mounts deliberately carried no Guard — a vanished source aborts the
// job rather than being skipped.
func TestDispatch_EmbeddedSkills_LeaveNoPerSkillBindOnTheLaunchedSpec(t *testing.T) {
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
	if len(be.launched) != 1 {
		t.Fatalf("backend received %d Launch calls, want exactly 1", len(be.launched))
	}

	for _, m := range be.launched[0].Mounts {
		for _, name := range skills.EmbeddedSkillNames() {
			for _, root := range skillDiscoveryRoots {
				if m.Target == filepath.Join(hostHomeDir(), root, name) {
					t.Errorf("the launched sandbox.Spec still binds %q from %q; embedded skills come from the image by symlink now", m.Target, m.Source)
				}
			}
		}
	}
}

// TestDispatch_HomeSkeletonDirs_ReachTheLaunchedSpec is the Dispatch-level half
// of the seam TestBuildSandboxSpec_HomeSkeletonDirs_* covers from the builder
// side (PR6). BuildSandboxSpec derives the field from the SandboxRuntimeInfo it
// is handed, so a Dispatch that stopped filling WorkspaceHomeVolume would
// silently produce a spec with nothing to verify — and the job container's
// check would then pass on every home, including a poisoned one, with nothing
// anywhere reporting it.
//
// The expected set is every discovery root and its parent, with no per-skill
// leaves: those are symlinks now rather than bind targets, which is what
// shrank this set to a constant of skillDiscoveryRoots — see
// workspaceHomeSkeletonDirs.
func TestDispatch_HomeSkeletonDirs_ReachTheLaunchedSpec(t *testing.T) {
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
	if len(be.launched) != 1 {
		t.Fatalf("backend received %d Launch calls, want exactly 1", len(be.launched))
	}

	launched := be.launched[0]
	if launched.HomeSkeletonRoot != hostHomeDir() {
		t.Errorf("the launched sandbox.Spec carries HomeSkeletonRoot = %q, want the sandbox $HOME %q",
			launched.HomeSkeletonRoot, hostHomeDir())
	}
	if !equalStringSets(launched.HomeSkeletonDirs, workspaceHomeSkeletonDirs()) {
		t.Errorf("the launched sandbox.Spec carries HomeSkeletonDirs = %v, want %v — the job container's skeleton check has nothing to verify without them",
			launched.HomeSkeletonDirs, workspaceHomeSkeletonDirs())
	}
}
