package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/skills"
)

// This file holds the Dispatch-level wiring guards for the embedded-skills
// half of docs/plans/workspace-home-volume-persistence.md 論点 e-2 (PR3):
// proof that Runner.Dispatch materializes the embedded skill set under the
// host-visible runtimes root — NOT into the workspace home, which PR6 turned
// into a named volume the daemon cannot write to — that the per-skill bind
// targets exist inside the workspace home by the time a job launches, and that
// a materialize failure fails the dispatch the same way an init.sh failure does
// (failJob + cleanup + error return).
//
// The pre-PR3 version of this file pinned the opposite of the first
// property: it asserted <home>/.claude/skills/boid-task/SKILL.md existed
// after a dispatch, because Phase 4 PR3 (docs/plans/home-workspace-volume.md)
// copy-synced the skill CONTENT into the workspace home. That is exactly the
// write PR3 relocates, so the assertion is inverted here rather than dropped.
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

// TestDispatch_SkillsSync_MaterializesUnderRuntimesRoot proves the embedded
// skill CONTENT lands under <RuntimesDir>/skills — the host-visible,
// per-installation directory the job container binds from — and nowhere
// inside the workspace home.
func TestDispatch_SkillsSync_MaterializesUnderRuntimesRoot(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	root := t.TempDir()
	runtimesDir := filepath.Join(root, "runtimes")

	r := skillsSyncTestRunner(t, runtimesDir, "myws")

	jobID, err := r.Dispatch(context.Background(), skillsSyncTestSpec(), nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if jobID == "" {
		t.Fatal("expected a non-empty job ID from a successful Dispatch")
	}

	skillFile := filepath.Join(runtimesDir, "skills", "boid-task", "SKILL.md")
	content, err := os.ReadFile(skillFile)
	if err != nil {
		t.Fatalf("read materialized skill file at %s: %v", skillFile, err)
	}
	if !strings.Contains(string(content), "boid-task") {
		t.Errorf("materialized SKILL.md missing expected content: %s", content)
	}

	// The workspace home must NOT receive the content any more: PR6 makes it
	// a named volume the daemon cannot write into, and a lingering copy-sync
	// would be a silent no-op there instead of an error.
	homeCopy := filepath.Join(root, "homes", "myws", ".claude", "skills", "boid-task", "SKILL.md")
	if _, err := os.Stat(homeCopy); !os.IsNotExist(err) {
		t.Errorf("embedded skill content was still written into the workspace home at %s (stat err=%v)", homeCopy, err)
	}
}

// TestDispatch_SkillsSync_CreatesPerSkillBindTargetsInWorkspaceHome pins that
// a dispatch leaves <home>/.claude and <home>/.claude/skills/<name> in place
// for every embedded skill, so the engine never gets to auto-create the
// intermediate ~/.claude as uid 0 when it realizes the per-skill bind (論点
// b-2's measured failure mode — a root-owned ~/.claude means the harness can no
// longer write ~/.claude/.credentials.json).
//
// PR3 satisfied this with a daemon-side mkdir on every dispatch. PR6 satisfies
// it from inside the init container instead, because the home is a named volume
// the daemon cannot write to — so the assertion is unchanged and deliberately
// says nothing about WHICH side created them: it is the property that has to
// survive the move, and the point of keeping it here is that the move must not
// quietly lose it.
func TestDispatch_SkillsSync_CreatesPerSkillBindTargetsInWorkspaceHome(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	root := t.TempDir()
	runtimesDir := filepath.Join(root, "runtimes")

	r := skillsSyncTestRunner(t, runtimesDir, "myws")
	be, ok := r.Backend.(*gwFakeBackend)
	if !ok {
		t.Fatalf("test wiring: Runner.Backend is %T, want *gwFakeBackend", r.Backend)
	}

	if _, err := r.Dispatch(context.Background(), skillsSyncTestSpec(), nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	homeDir := be.workspaceHomeDir(dockerres.WorkspaceHomeVolumeName(r.InstallID, "myws"))
	claudeDir := filepath.Join(homeDir, ".claude")
	if info, err := os.Stat(claudeDir); err != nil || !info.IsDir() {
		t.Fatalf("stat %s: info=%v err=%v, want an existing directory", claudeDir, info, err)
	}
	names := skills.EmbeddedSkillNames()
	if len(names) == 0 {
		t.Fatal("EmbeddedSkillNames() returned nothing; the bind-target assertion below would be vacuous")
	}
	for _, name := range names {
		target := filepath.Join(claudeDir, "skills", name)
		info, err := os.Stat(target)
		if err != nil || !info.IsDir() {
			t.Errorf("bind target %s: info=%v err=%v, want an existing directory", target, info, err)
		}
	}
}

// TestDispatch_EmbeddedSkills_ReachLaunchedSpecAsPerSkillReadOnlyBinds is the
// wiring-seam guard that joins the two halves of PR3 into one chain:
// Runner.Dispatch materializes the embedded skill set, threads the directory
// it materialized into through SandboxRuntimeInfo.SkillsSourceDir, and
// homeMounts turns that into one read-only bind per skill inside the sandbox's
// $HOME — asserted against the sandbox.Spec the BACKEND actually received.
//
// Neither of the tests either side of it can see that chain break, which is
// exactly the "both ends wired, never crossed" class .claude/skills/
// boid-review's Lens 1 names:
//
//   - TestDispatch_SkillsSync_MaterializesUnderRuntimesRoot only looks at
//     files on disk, and the materialize step writes those whether or not its
//     return value is ever used.
//   - TestBuildSandboxSpec_SkillsSourceDir_EndToEndPerSkillBinds (sandbox_
//     builder_test.go) hand-builds the SandboxRuntimeInfo, so it proves the
//     builder maps the field but not that Dispatch fills it.
//
// Deleting `SkillsSourceDir: skillsSourceDir` from Dispatch's rtInfo literal
// therefore leaves both of them green while every job silently starts with no
// /boid-task, /boid-orchestrate or /boid-web at all — a failure that surfaces
// only as an agent that cannot find its own skills, with nothing in any log.
// This case is the one that goes red. (It is also the same shape as PR2's
// TestWire_DataHomeDir_ReachesMarkerAndLockOnDisk, for the same reason.)
//
// Each mount's Source is additionally required to hold a real materialized
// SKILL.md, so a spec pointing at a directory nothing was ever written into —
// a plausible outcome of embeddedSkillsDir and skills.DeployAll drifting apart
// — fails here too rather than passing a pure string comparison.
func TestDispatch_EmbeddedSkills_ReachLaunchedSpecAsPerSkillReadOnlyBinds(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	root := t.TempDir()
	runtimesDir := filepath.Join(root, "runtimes")

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

	// hostHomeDir() is what BuildSandboxSpec uses as the sandbox's $HOME, and
	// setupWorkspaceHomeTestDirs has already pointed it at a temp dir.
	homeDir := hostHomeDir()
	if homeDir == "" {
		t.Fatal("test wiring: hostHomeDir() is empty, so the expected bind targets cannot be computed")
	}
	names := skills.EmbeddedSkillNames()
	if len(names) == 0 {
		t.Fatal("EmbeddedSkillNames() returned nothing; every assertion below would be vacuous")
	}

	for _, name := range names {
		target := filepath.Join(homeDir, ".claude", "skills", name)
		idx := mountTargetIndex(launched.Mounts, target)
		if idx == -1 {
			t.Fatalf("the launched sandbox.Spec has no mount at %s — Dispatch did not thread the materialized skills dir into SandboxRuntimeInfo.SkillsSourceDir, or homeMounts stopped emitting the per-skill binds; mounts=%+v",
				target, launched.Mounts)
		}
		m := launched.Mounts[idx]

		wantSource := filepath.Join(runtimesDir, "skills", name)
		if m.Source != wantSource {
			t.Errorf("mount for skill %q has Source %q, want %q (the <RuntimesDir>/skills tree Dispatch just materialized)", name, m.Source, wantSource)
		}
		if m.Type != sandbox.MountBind {
			t.Errorf("mount for skill %q has Type %v, want a bind", name, m.Type)
		}
		if !m.ReadOnly {
			t.Errorf("mount for skill %q is not read-only: %+v — a job could then rewrite the per-installation skill set every other job shares", name, m)
		}

		// The source is not merely a plausible-looking path: the materialize
		// step really put the skill there.
		skillFile := filepath.Join(m.Source, "SKILL.md")
		if _, err := os.Stat(skillFile); err != nil {
			t.Errorf("mount source for skill %q does not hold a materialized SKILL.md (%s): %v", name, skillFile, err)
		}
	}
}

// TestDispatch_SkillsMaterializeFails_MarksJobFailedAndCallsCleanup is the
// Dispatch-level guard for the materialize error path. It forces
// skills.DeployAll to fail deterministically by pre-creating a plain *file*
// at <RuntimesDir>/skills so the symlink-safe directory walk hits ENOTDIR,
// and asserts Dispatch follows the same failJob + cleanup + error-return
// pattern as every other pre-BuildSandboxSpec dispatch error (e.g. an
// init.sh failure — see
// TestDispatch_WorkspaceHomeInitFails_MarksJobFailedAndCallsCleanup in
// workspace_home_dispatch_test.go).
//
// The pre-PR3 version of this case induced the same ENOTDIR one directory
// over, at <home>/.claude, because that was where DeployAll wrote. The
// property being pinned — "a skills sync failure fails the dispatch loudly
// rather than letting a job start against a stale or missing skill set" — is
// unchanged; only the directory it is induced in moved.
func TestDispatch_SkillsMaterializeFails_MarksJobFailedAndCallsCleanup(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	root := t.TempDir()
	runtimesDir := filepath.Join(root, "runtimes")
	if err := os.MkdirAll(runtimesDir, 0o700); err != nil {
		t.Fatalf("mkdir runtimes dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runtimesDir, "skills"), []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	r := skillsSyncTestRunner(t, runtimesDir, "myws")

	assertDispatchFailsWithCleanup(t, r,
		"materialize embedded skills into",
		filepath.Join(runtimesDir, "skills"),
		"not a directory")
}

// --- PR6: creating and verifying the bind targets both left this package ----
//
// PR3 had Runner.Dispatch mkdir the per-skill bind targets inside the workspace
// home on every dispatch and verify, with an fstat, that the daemon owned each
// one. PR6 turned the home into a docker named volume, which the daemon can
// neither write to nor stat, so the five cases that used to live here
// (SkillsBindTargetPrepFails / NotDaemonOwned / ForeignOwnerPerComponent /
// ForeignOwnerRecoveryIsAvailable / OwnershipCheckAcceptsDaemonOwned) moved
// rather than being dropped:
//
//   - CREATION is the init container's builtin prelude, gated on a completion
//     marker that now records the skeleton set — so a release that adds an
//     embedded skill re-prepares the home instead of leaving the engine to
//     auto-create the new bind target as uid 0. Pinned by
//     TestResolveWorkspaceHome_SkeletonSetChanged_ReInitializes and
//     TestResolveWorkspaceHome_MarkerRecordsTheVolumeIdentityAndSkeletonSet
//     (workspace_home_volume_test.go), and by the prelude's own real-bash
//     tests in workspace_init_test.go.
//   - VERIFICATION is the job container's own runner, which checks
//     sandbox.Spec.HomeSkeletonDirs before the harness starts. 論点 e-2 requires
//     the check to sit outside whatever creates those directories; the runner
//     also runs on every dispatch, AFTER the engine's auto-creation rather than
//     before it, and as the very uid whose write access is in question. Pinned
//     by internal/sandbox/runner's home_skeleton_linux_test.go, and the wiring
//     from workspaceHomeSkeletonDirs into the spec by
//     TestBuildSandboxSpec_HomeSkeletonDirs_* (sandbox_builder_test.go).
//
// The recovery instructions the daemon-side error carried moved with the check
// (see runner.verifyHomeSkeleton), including the part PR3's codex review
// established: `boid workspace remove` is not a usable primary instruction.

// assertDispatchFailsWithCleanup runs one dispatch that is expected to fail
// during the embedded-skills step and asserts the shared failure contract: no
// job ID returned, the cleanup callback invoked, exactly one failed job row,
// and — via wantErrParts — that the failure is the one the calling case set
// out to induce.
//
// wantErrParts is asserted against both the returned error and the failed
// job's Output, and both are required to be non-empty. The pre-review version
// of this helper checked only that Output was non-empty, which every
// pre-BuildSandboxSpec failure in Runner.Dispatch satisfies: an unrelated
// regression in resolveProjectRuntime, resolveWorkspaceHome or the init.sh
// run would have kept all four of these cases green while the step each one
// names went entirely unexercised (codex review of PR3). Each case therefore
// passes the phrase identifying its step, the exact path it planted the
// obstruction at, and the errno text that obstruction produces — the three
// together are not reachable by any other failure in this function.
func assertDispatchFailsWithCleanup(t *testing.T, r *Runner, wantErrParts ...string) {
	t.Helper()
	if len(wantErrParts) == 0 {
		t.Fatal("assertDispatchFailsWithCleanup needs at least one expected error fragment; a bare 'it failed somehow' assertion passes on unrelated failures")
	}

	var cleanupCalled bool
	jobID, err := r.Dispatch(context.Background(), skillsSyncTestSpec(), func() { cleanupCalled = true })
	if err == nil {
		t.Fatal("expected Dispatch to fail when the embedded skills step errors")
	}
	if jobID != "" {
		t.Errorf("jobID = %q, want empty on failure", jobID)
	}
	if !cleanupCalled {
		t.Error("cleanup callback was not called on embedded skills error")
	}
	for _, want := range wantErrParts {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Dispatch error does not mention %q, so this case did not induce the failure it names; got: %v", want, err)
		}
	}

	jobs, listErr := ListJobsFiltered(r.DB, JobFilter{Status: string(JobStatusFailed)})
	if listErr != nil {
		t.Fatalf("list failed jobs: %v", listErr)
	}
	if len(jobs) != 1 {
		t.Fatalf("want 1 failed job, got %d", len(jobs))
	}
	// The job row is what a user actually sees (TUI / Web / `boid job log`),
	// so the diagnosis has to survive the trip through failJob, not merely
	// exist in the error Dispatch returned to its caller.
	for _, want := range wantErrParts {
		if !strings.Contains(jobs[0].Output, want) {
			t.Errorf("failed job Output does not mention %q, so the failure reaches the DB row stripped of its diagnosis; got: %s", want, jobs[0].Output)
		}
	}
}

// TestDispatch_HomeSkeletonDirs_ReachTheLaunchedSpec is the Dispatch-level half
// of the seam TestBuildSandboxSpec_HomeSkeletonDirs_* covers from the builder
// side (PR6). BuildSandboxSpec derives the field from the SandboxRuntimeInfo it
// is handed, so a Dispatch that stopped filling WorkspaceHomeVolume or
// SkillsSourceDir would silently produce a spec with nothing to verify — and
// the job container's check would then pass on every home, including a poisoned
// one, with nothing anywhere reporting it.
//
// The expected set is the bind-target ANCESTORS only. The per-skill leaves are
// excluded by homeSkeletonDirs because this same spec covers each of them with
// a read-only bind, which is what an os.Stat inside the container would then be
// reading — see that function for the exclusion and what it leaves exposed.
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
	want := []string{".claude", filepath.Join(".claude", "skills")}
	if !equalStringSets(launched.HomeSkeletonDirs, want) {
		t.Errorf("the launched sandbox.Spec carries HomeSkeletonDirs = %v, want %v — the job container's skeleton check has nothing to verify without them",
			launched.HomeSkeletonDirs, want)
	}
}
