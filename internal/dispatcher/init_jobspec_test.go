package dispatcher

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestBuildInitJobSpec_InstructionPopulatesBoidUserAnswer(t *testing.T) {
	want := "boid kit init を実行して"
	spec := BuildInitJobSpec(InitJobInput{
		Profile:     sandbox.ProfileInit,
		Argv:        []string{"boid-kit-init"},
		HarnessType: "claude",
		Instruction: want,
	})
	got, ok := spec.Env["BOID_USER_ANSWER"]
	if !ok {
		t.Fatalf("BOID_USER_ANSWER not set; Env=%v", spec.Env)
	}
	if got != want {
		t.Fatalf("BOID_USER_ANSWER = %q, want %q", got, want)
	}
}

func TestBuildInitJobSpec_NoInstructionLeavesEnvUntouched(t *testing.T) {
	spec := BuildInitJobSpec(InitJobInput{
		Profile:     sandbox.ProfileInit,
		Argv:        []string{"boid-kit-init"},
		HarnessType: "claude",
		Env:         map[string]string{"BOID_WORKSPACE_SLUG": "default"},
		// Instruction intentionally omitted.
	})
	if _, ok := spec.Env["BOID_USER_ANSWER"]; ok {
		t.Fatalf("BOID_USER_ANSWER should not be set without an Instruction; Env=%v", spec.Env)
	}
	if spec.Env["BOID_WORKSPACE_SLUG"] != "default" {
		t.Fatalf("caller-supplied env entry was clobbered; Env=%v", spec.Env)
	}
}

func TestBuildInitJobSpec_InstructionPreservesCallerEnv(t *testing.T) {
	spec := BuildInitJobSpec(InitJobInput{
		Profile:     sandbox.ProfileInit,
		Argv:        []string{"boid-workspace-configure"},
		HarnessType: "claude",
		Env:         map[string]string{"BOID_WORKSPACE_SLUG": "khi"},
		Instruction: `workspace "khi" の boid workspace configure を実行して`,
	})
	if spec.Env["BOID_WORKSPACE_SLUG"] != "khi" {
		t.Fatalf("caller-supplied env clobbered: %v", spec.Env)
	}
	if spec.Env["BOID_USER_ANSWER"] == "" {
		t.Fatalf("BOID_USER_ANSWER should be set: %v", spec.Env)
	}
}
