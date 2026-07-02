package dispatcher

import (
	"reflect"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// These tests cover the two task-less JobSpec builders — the `boid agent`
// (BuildSessionJobSpec) and `boid exec` (BuildExecJobSpec) entry points. They
// are the seam where a user-initiated launch is turned into a JobSpec; the
// downstream BuildSandboxSpec tests hand-build their specs and therefore
// cannot see a builder that stamps the wrong HarnessType or drops a binding.
// The 2026-06-29 binding regression and the Phase 3-d "HarnessType 設定漏れ"
// exec-127 bug both lived at exactly this layer, so each field-level contract
// is asserted directly.

// sampleSessionInput returns a fully-populated SessionJobInput so each test can
// tweak one field and assert its effect without re-specifying the rest.
func sampleSessionInput() SessionJobInput {
	return SessionJobInput{
		ProjectID:      "proj-1",
		ProjectWorkDir: "/work/proj-1",
		HarnessType:    "claude",
		AdditionalBindings: []orchestrator.BindMount{
			{Source: "/opt/volta", Target: "/opt/volta", Mode: "rw"},
		},
		KitRoots:        []string{"/host/kits/volta"},
		SecretNamespace: "ns-1",
		DockerEnabled:   true,
	}
}

// TestBuildSessionJobSpec_FieldContracts pins the `boid agent` builder's core
// output: HarnessType passthrough, the Session kind, an always-on PTY, and the
// verbatim carry-through of the project-trait overlay (bindings / kit roots /
// secret namespace / docker) into Visibility. A drop in any of these silently
// changes what the agent session sees inside the sandbox.
func TestBuildSessionJobSpec_FieldContracts(t *testing.T) {
	in := sampleSessionInput()
	spec := BuildSessionJobSpec(in)

	if spec.HarnessType != "claude" {
		t.Errorf("HarnessType = %q, want claude (passthrough)", spec.HarnessType)
	}
	if spec.Kind != orchestrator.JobKindSession {
		t.Errorf("Kind = %v, want %v", spec.Kind, orchestrator.JobKindSession)
	}
	if !spec.Interactive {
		t.Error("Interactive = false, want true (sessions are PTY-attached by definition)")
	}
	if spec.ProjectID != in.ProjectID {
		t.Errorf("ProjectID = %q, want %q", spec.ProjectID, in.ProjectID)
	}
	if spec.Visibility.ProjectDir != in.ProjectWorkDir {
		t.Errorf("Visibility.ProjectDir = %q, want %q", spec.Visibility.ProjectDir, in.ProjectWorkDir)
	}
	if spec.Visibility.UseWorktree {
		t.Error("Visibility.UseWorktree = true, want false (sessions never use a worktree)")
	}
	if !reflect.DeepEqual(spec.Visibility.AdditionalBindings, in.AdditionalBindings) {
		t.Errorf("Visibility.AdditionalBindings = %+v, want %+v (must carry the project/kit binding overlay)", spec.Visibility.AdditionalBindings, in.AdditionalBindings)
	}
	if !reflect.DeepEqual(spec.Visibility.KitRoots, in.KitRoots) {
		t.Errorf("Visibility.KitRoots = %+v, want %+v", spec.Visibility.KitRoots, in.KitRoots)
	}
	if spec.SecretNamespace != in.SecretNamespace {
		t.Errorf("SecretNamespace = %q, want %q", spec.SecretNamespace, in.SecretNamespace)
	}
	if !spec.Visibility.DockerEnabled {
		t.Error("Visibility.DockerEnabled = false, want true")
	}
}

// TestBuildSessionJobSpec_WritableFollowsReadonly pins the fail-safe default:
// sessions are writable unless the caller opts into read-only. Writable is what
// gates writes to the project dir inside the sandbox.
func TestBuildSessionJobSpec_WritableFollowsReadonly(t *testing.T) {
	for _, tc := range []struct {
		readonly     bool
		wantWritable bool
	}{
		{readonly: false, wantWritable: true},
		{readonly: true, wantWritable: false},
	} {
		in := sampleSessionInput()
		in.Readonly = tc.readonly
		spec := BuildSessionJobSpec(in)
		if spec.Visibility.Writable != tc.wantWritable {
			t.Errorf("Readonly=%v: Visibility.Writable = %v, want %v", tc.readonly, spec.Visibility.Writable, tc.wantWritable)
		}
	}
}

// TestBuildSessionJobSpec_InstructionAndModelToEnv pins the two env plumbings a
// session relies on: --instruction becomes BOID_USER_ANSWER (delivered as the
// agent's first turn via RunContext.UserAnswer) and --model becomes BOID_MODEL.
func TestBuildSessionJobSpec_InstructionAndModelToEnv(t *testing.T) {
	in := sampleSessionInput()
	in.Instruction = "do the thing"
	in.Model = "claude-opus-4-8"
	spec := BuildSessionJobSpec(in)

	if got := spec.Env["BOID_USER_ANSWER"]; got != "do the thing" {
		t.Errorf("Env[BOID_USER_ANSWER] = %q, want %q", got, "do the thing")
	}
	if got := spec.Env["BOID_MODEL"]; got != "claude-opus-4-8" {
		t.Errorf("Env[BOID_MODEL] = %q, want %q", got, "claude-opus-4-8")
	}
}

// TestBuildSessionJobSpec_DisplayNameFallback pins the "<harness> session"
// fallback used when the caller supplies no display name.
func TestBuildSessionJobSpec_DisplayNameFallback(t *testing.T) {
	in := sampleSessionInput()
	in.DisplayName = ""
	if got := BuildSessionJobSpec(in).DisplayName; got != "claude session" {
		t.Errorf("DisplayName = %q, want %q", got, "claude session")
	}

	in.DisplayName = "my session"
	if got := BuildSessionJobSpec(in).DisplayName; got != "my session" {
		t.Errorf("DisplayName = %q, want %q (explicit name must win)", got, "my session")
	}
}

// TestBuildExecJobSpec_ForcesShellHarness is the critical `boid exec` contract:
// whatever HarnessType the caller passes, exec MUST run under the shell adapter.
// A regression to passthrough here would route a plain command through a real
// agent adapter (the class of bug that produced the Phase 3-d exec-127 guard).
func TestBuildExecJobSpec_ForcesShellHarness(t *testing.T) {
	in := sampleSessionInput()
	in.HarnessType = "claude" // must be ignored/overridden
	argv := []string{"/bin/echo", "hi"}

	spec := BuildExecJobSpec(in, argv, false)

	if spec.HarnessType != "shell" {
		t.Errorf("HarnessType = %q, want shell (exec forces shell regardless of input)", spec.HarnessType)
	}
	if spec.Kind != orchestrator.JobKindExec {
		t.Errorf("Kind = %v, want %v", spec.Kind, orchestrator.JobKindExec)
	}
	if !reflect.DeepEqual(spec.Argv, argv) {
		t.Errorf("Argv = %v, want %v", spec.Argv, argv)
	}
	if spec.Interactive {
		t.Error("Interactive = true, want false (passed interactive=false must win)")
	}
}

// TestBuildExecJobSpec_KeepsBindings guards that exec inherits the project /
// kit binding overlay. `boid exec` in a project with kit-provided CLIs must
// still see those binds — the shell-harness path (no harness bindings of its
// own) is exactly the case that would NOT have caught the 2026-06-29 exclusive-
// replace regression, so its own guard matters.
func TestBuildExecJobSpec_KeepsBindings(t *testing.T) {
	in := sampleSessionInput()
	spec := BuildExecJobSpec(in, []string{"/bin/true"}, true)

	if !reflect.DeepEqual(spec.Visibility.AdditionalBindings, in.AdditionalBindings) {
		t.Errorf("Visibility.AdditionalBindings = %+v, want %+v (exec must keep the binding overlay)", spec.Visibility.AdditionalBindings, in.AdditionalBindings)
	}
	if !reflect.DeepEqual(spec.Visibility.KitRoots, in.KitRoots) {
		t.Errorf("Visibility.KitRoots = %+v, want %+v", spec.Visibility.KitRoots, in.KitRoots)
	}
	if !spec.Interactive {
		t.Error("Interactive = false, want true (passed interactive=true must win)")
	}
}

// TestBuildExecJobSpec_DisplayNameFallsBackToArgv0 pins the exec-specific label
// fallback (argv[0]) used when the caller leaves DisplayName empty.
func TestBuildExecJobSpec_DisplayNameFallsBackToArgv0(t *testing.T) {
	in := sampleSessionInput()
	in.DisplayName = ""
	spec := BuildExecJobSpec(in, []string{"/usr/bin/make", "build"}, false)
	if spec.DisplayName != "/usr/bin/make" {
		t.Errorf("DisplayName = %q, want %q (argv[0] fallback)", spec.DisplayName, "/usr/bin/make")
	}
}
