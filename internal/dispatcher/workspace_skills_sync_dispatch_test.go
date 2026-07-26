package dispatcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/skills"
)

// This file holds the Dispatch-level wiring guards for the embedded-skills
// half of docs/plans/workspace-home-volume-persistence.md 論点 e-2 (PR3):
// proof that Runner.Dispatch materializes the embedded skill set under the
// host-visible runtimes root — NOT into the workspace home, which PR6 turns
// into a named volume the daemon cannot write to — that it prepares the
// per-skill bind targets inside the workspace home, and that a failure of
// either step fails the dispatch the same way an init.sh failure does
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

// TestDispatch_SkillsSync_CreatesPerSkillBindTargetsInWorkspaceHome pins the
// other half of PR3: the daemon explicitly creates <home>/.claude and
// <home>/.claude/skills/<name> for every embedded skill, so the engine never
// gets to auto-create the intermediate ~/.claude as uid 0 when it realizes
// the per-skill bind (論点 b-2's measured failure mode — a root-owned
// ~/.claude means the harness can no longer write ~/.claude/.credentials.json).
func TestDispatch_SkillsSync_CreatesPerSkillBindTargetsInWorkspaceHome(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	root := t.TempDir()
	runtimesDir := filepath.Join(root, "runtimes")

	r := skillsSyncTestRunner(t, runtimesDir, "myws")

	if _, err := r.Dispatch(context.Background(), skillsSyncTestSpec(), nil); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	claudeDir := filepath.Join(root, "homes", "myws", ".claude")
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

// TestDispatch_SkillsBindTargetPrepFails_MarksJobFailedAndCallsCleanup pins
// that the bind-target mkdir PR3 adds is held to the same fail-loud contract
// as the materialize step. A dispatch that could not prepare
// <home>/.claude/skills/<name> would otherwise hand the engine a missing
// bind target and get a root-owned ~/.claude (論点 b-2), which shows up much
// later as an unwritable ~/.claude/.credentials.json.
//
// Induced exactly the way the pre-PR3 sync-failure case was: a plain file at
// <home>/.claude makes every mkdir underneath it fail with ENOTDIR.
//
// PR5 changed WHEN this test has to plant that file. The init container's
// builtin prelude now creates the same skeleton (workspaceHomeSkeletonDirs),
// so on a FRESH workspace the blocker is hit there first and the dispatch
// fails — loudly, but with the prelude's message rather than this one. The
// per-dispatch bind-target step is what runs for an ALREADY-INITIALIZED home,
// which is also the only state in which the condition it detects can arise
// (a job of this workspace replacing ~/.claude after prep finished — see
// syncEmbeddedSkills' doc comment on the window that is deliberately left
// open). So the home is initialized first, and only then blocked.
func TestDispatch_SkillsBindTargetPrepFails_MarksJobFailedAndCallsCleanup(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	root := t.TempDir()
	runtimesDir := filepath.Join(root, "runtimes")

	r := skillsSyncTestRunner(t, runtimesDir, "")

	// Initialize the home for real, so the next resolve short-circuits on the
	// completion marker and the prelude does not run again.
	homeDir, _, err := r.resolveWorkspaceHome(context.Background(), "")
	if err != nil {
		t.Fatalf("initialize the workspace home: %v", err)
	}
	claudeDir := filepath.Join(homeDir, ".claude")
	if err := os.RemoveAll(claudeDir); err != nil {
		t.Fatalf("remove the prepared .claude: %v", err)
	}
	if err := os.WriteFile(claudeDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}

	assertDispatchFailsWithCleanup(t, r,
		"prepare skill bind target",
		claudeDir,
		"not a directory")
}

// TestDispatch_SkillsBindTargetNotDaemonOwned_FailsDispatch pins the one
// consequence of the bind-target TOCTOU that is NOT recoverable, and is
// therefore the only part of it PR3 closes.
//
// The window itself stays open by decision (see syncEmbeddedSkills' doc
// comment): a concurrent job of the same workspace holds the same workspace
// HOME rw, so between this dispatch's mkdir and its container's launch that
// job can rename or delete ~/.claude, and the engine then auto-creates the
// missing bind-target path as uid 0. What that leaves behind is not a failed
// job — it is a workspace HOME with a root-owned ~/.claude, which neither the
// uid-1000 harness nor the daemon (uid 1000, or a rootless-podman host subuid
// away from the engine's uid 0) can chown back. Every later dispatch against
// that workspace then starts fine and silently fails to persist
// ~/.claude/.credentials.json. Detecting the poisoned directory at the next
// dispatch and refusing to run converts that permanent, silent breakage into
// one loud, diagnosable failure.
//
// A test cannot chown a directory to a foreign uid without privileges, so the
// comparison is faked from the daemon's side rather than the directory's: the
// fstat still runs for real against a real directory the test process really
// owns, and only "which uid the daemon believes it is" moves. That exercises
// the same inequality the real case produces, and keeps the assertion free of
// any hard-coded uid — the property is "not owned by this process", not "owned
// by 0".
func TestDispatch_SkillsBindTargetNotDaemonOwned_FailsDispatch(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	root := t.TempDir()
	runtimesDir := filepath.Join(root, "runtimes")

	realUID := os.Getuid()
	restore := daemonUID
	daemonUID = func() int { return realUID + 1 }
	t.Cleanup(func() { daemonUID = restore })

	r := skillsSyncTestRunner(t, runtimesDir, "myws")

	// The very first bind target prepared is <home>/.claude, so that is the
	// path named in the error — pinning it proves the check runs before any
	// deeper target is touched, not merely somewhere in the loop.
	assertDispatchFailsWithCleanup(t, r,
		filepath.Join(root, "homes", "myws", ".claude"),
		"所有者",
		fmt.Sprintf("uid %d", realUID),
		"workspace HOME")
}

// poisonBindTargetOwner makes prepareBindTarget see exactly ONE directory —
// the one at poisoned — as owned by somebody other than this daemon, for the
// duration of t, and returns the foreign uid it will report.
//
// The real skills.MkdirAllNoSymlink still runs for every path, poisoned or
// not: its mkdir really happens, and so does its fstat on the descriptor the
// symlink-checked walk ended on. Only the uid ANSWER is rewritten, and only
// for one path. What that reproduces is the condition a test cannot otherwise
// produce at all — chown(2) to a foreign uid needs privileges no test has —
// while leaving every other component of the same dispatch genuinely
// daemon-owned, which is what makes "this component specifically is checked"
// observable.
//
// It complements, rather than replaces, the daemonUID seam that
// TestDispatch_SkillsBindTargetNotDaemonOwned_FailsDispatch uses above: that
// one moves the DAEMON's side of the comparison against a real, really-fstat'ed
// directory, which is what pins the comparison to this process's own uid rather
// than a hard-coded 1000 (a hard-coded literal survives THIS seam, because a
// test running as uid 1000 would still see the injected 1001 rejected). This
// one moves the DIRECTORY's side per path, which is the only way to say which
// component was checked. Neither axis alone covers both properties.
func poisonBindTargetOwner(t *testing.T, poisoned string) int {
	t.Helper()
	realOwner := bindTargetOwnerUID
	foreign := os.Getuid() + 1
	bindTargetOwnerUID = func(dir string) (int, error) {
		uid, err := realOwner(dir)
		if err != nil || dir != poisoned {
			return uid, err
		}
		return foreign, nil
	}
	t.Cleanup(func() { bindTargetOwnerUID = realOwner })
	return foreign
}

// TestDispatch_SkillsBindTargetForeignOwner_FailsPerComponent pins that EVERY
// component of the per-skill bind target path is individually ownership-
// checked, not just the first one.
//
// This is the gap the single daemonUID-seam case above could not cover: that
// seam moves the daemon's believed uid globally, so every component fails at
// once and the dispatch reports the first — <home>/.claude. Deleting the
// explicit prepareBindTarget call for <home>/.claude/skills, or the per-skill
// loop's own check, left it green (verified by mutation). That mattered because the engine's auto-creation
// of a missing bind target creates the WHOLE missing path as uid 0, not merely
// its leaf (docs/plans/workspace-home-volume-persistence.md 論点 b-2), so any
// component of it can be the poisoned one — and an unchecked component is a
// silently unwritable ~/.claude for every later dispatch of that workspace.
//
// The leaf case deliberately poisons the LAST embedded skill, so the case also
// fails if the loop stops early instead of covering every name.
func TestDispatch_SkillsBindTargetForeignOwner_FailsPerComponent(t *testing.T) {
	names := skills.EmbeddedSkillNames()
	if len(names) == 0 {
		t.Fatal("EmbeddedSkillNames() returned nothing; the leaf case below would be vacuous")
	}

	cases := []struct {
		name string
		// rel is the poisoned directory's path relative to the workspace home.
		rel []string
	}{
		{name: "claude dir", rel: []string{".claude"}},
		{name: "skills dir", rel: []string{".claude", "skills"}},
		{name: "last skill leaf", rel: []string{".claude", "skills", names[len(names)-1]}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setupWorkspaceHomeTestDirs(t)
			root := t.TempDir()
			runtimesDir := filepath.Join(root, "runtimes")

			poisoned := filepath.Join(append([]string{root, "homes", "myws"}, tc.rel...)...)
			foreign := poisonBindTargetOwner(t, poisoned)

			r := skillsSyncTestRunner(t, runtimesDir, "myws")

			// %q, not the bare path: the leaf targets have the two directory
			// targets as string PREFIXES, so a bare-path assertion for
			// ".../.claude/skills" would also be satisfied by a failure at
			// ".../.claude/skills/boid-task". The closing quote is what makes
			// each case name exactly one component.
			assertDispatchFailsWithCleanup(t, r,
				"prepare skill bind target",
				fmt.Sprintf("%q", poisoned),
				"所有者",
				fmt.Sprintf("uid %d", foreign))
		})
	}
}

// TestPrepareBindTarget_ForeignOwnerRecoveryIsAvailable pins that the recovery
// this error hands the operator is one that actually exists.
//
// The message's whole value is that it converts a permanently, silently broken
// workspace HOME into one diagnosable failure — which it only does if the steps
// it names can be carried out. Its first version pointed at `boid workspace
// remove <slug>` as the way out, and that instruction is unavailable or
// incomplete on every path an operator can reach it from:
//
//   - `default` cannot be removed at all. Three layers reject the reserved
//     slug before anything is deleted (cmd/workspace.go's runWorkspaceRemove,
//     api.ProjectAppService.RemoveWorkspace, and
//     orchestrator.WorkspaceRepository.Remove), and internal/api/
//     workspace_homes.go's deleteWorkspaceHome skips the home directory for it
//     explicitly on top of that.
//   - For a non-default workspace, home deletion is best effort and reported,
//     not enforced: WorkspaceHandler.Remove deletes the DB row FIRST and keeps
//     the response 200 even when os.RemoveAll then fails — which is exactly
//     what a foreign-owned child directory (the very condition this error is
//     about) causes. The outcome is an orphaned home plus a vanished
//     workspace.
//   - Even on full success it removes the workspace itself, so the operator is
//     left having to recreate it and re-assign its projects.
//
// The assertions are therefore: the message must name a directly executable
// deletion of the offending directory, and if it mentions `workspace remove`
// at all it must carry all three caveats. A rewrite that drops the suggestion
// entirely stays green — the conditional is the point, since the requirement
// is honesty about that route, not its presence.
//
// Same shape as internal/skills/deploy_test.go's
// TestDeployAll_ErrorsDoNotNameACallerSpecificLocation: an error message with a
// contract gets a test on the contract.
func TestPrepareBindTarget_ForeignOwnerRecoveryIsAvailable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".claude")
	foreign := poisonBindTargetOwner(t, dir)

	err := prepareBindTarget(dir)
	if err == nil {
		t.Fatal("prepareBindTarget on a foreign-owned directory must fail")
	}
	msg := err.Error()

	// The directly executable route: delete the offending directory with the
	// privileges its owner requires. That one works on every workspace,
	// default included, and touches nothing else in the home.
	for _, want := range []string{
		fmt.Sprintf("%q", dir),
		fmt.Sprintf("uid %d", foreign),
		"rm -rf",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not mention %q, so it does not spell out a recovery the operator can actually run; got: %s", want, msg)
		}
	}

	if !strings.Contains(msg, "workspace remove") {
		return
	}
	for _, want := range []struct{ token, why string }{
		{orchestrator.DefaultWorkspaceSlug, "the reserved default workspace cannot be removed, and its home deletion is skipped outright"},
		{"確認", "home deletion is best-effort: the DB row is removed first and the response stays 200 even when the directory survives"},
		{"作り直", "a successful remove takes the workspace definition with it, so it has to be recreated and its projects re-assigned"},
	} {
		if !strings.Contains(msg, want.token) {
			t.Errorf("error recommends `workspace remove` without mentioning %q — %s; got: %s", want.token, want.why, msg)
		}
	}
}

// TestDispatch_SkillsBindTargetOwnershipCheck_AcceptsDaemonOwned is the
// negative control for the cases above: with neither seam moved, the very same
// dispatch must succeed. Without it, a check that rejected *every* bind target
// (an inverted comparison, say) would still make the failure cases pass.
func TestDispatch_SkillsBindTargetOwnershipCheck_AcceptsDaemonOwned(t *testing.T) {
	setupWorkspaceHomeTestDirs(t)
	root := t.TempDir()
	runtimesDir := filepath.Join(root, "runtimes")

	r := skillsSyncTestRunner(t, runtimesDir, "myws")

	if _, err := r.Dispatch(context.Background(), skillsSyncTestSpec(), nil); err != nil {
		t.Fatalf("Dispatch against bind targets the daemon itself created: %v", err)
	}
}

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
