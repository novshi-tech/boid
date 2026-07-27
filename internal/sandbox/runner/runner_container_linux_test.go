//go:build linux

package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestRunContainer_AppliesFilesSymlinksAndRunsAgent exercises RunContainer
// end to end (docs/plans/phase6-container-backend.md §PR2 / §決定 2): unlike
// RunInnerChild, RunContainer performs no mount/pivot_root syscalls at all
// (namespace isolation is delegated to the container runtime), so it can run
// as a plain, unprivileged Go test — no unshare/CAP_SYS_ADMIN required.
func TestRunContainer_AppliesFilesSymlinksAndRunsAgent(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "materialized.txt")
	linkPath := filepath.Join(dir, "bin", "gh")
	stdoutPath := filepath.Join(dir, "stdout.txt")
	specPath := filepath.Join(dir, "spec.json")
	statePath := filepath.Join(dir, "state.json")

	spec := sandbox.Spec{
		ID:          "container-e2e-1",
		HarnessType: sandbox.HarnessShell,
		Argv:        []string{"/bin/sh", "-c", "printf ran"},
		WorkDir:     dir,
		Env:         map[string]string{"PATH": "/usr/bin:/bin"},
		Files: []sandbox.FileWrite{
			{Path: filePath, Content: "hello"},
		},
		Symlinks: []sandbox.Symlink{
			{LinkPath: linkPath, LinkTarget: "boid"},
		},
		StdoutCaptureFile: stdoutPath,
		// Foreground so postJobDone's defer no-ops without a broker
		// listening — this test only exercises the entrypoint's own setup
		// + dispatch, not the broker RPC.
		Foreground: true,
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	exitCode, err := RunContainer(specPath, statePath)
	if err != nil {
		t.Fatalf("RunContainer: %v", err)
	}
	if exitCode != 0 {
		t.Fatalf("exitCode = %d, want 0", exitCode)
	}

	if got, err := os.ReadFile(filePath); err != nil || string(got) != "hello" {
		t.Errorf("spec.Files not materialized: content=%q err=%v", got, err)
	}
	if got, err := os.Readlink(linkPath); err != nil || got != "boid" {
		t.Errorf("spec.Symlinks not materialized: target=%q err=%v", got, err)
	}
	if got, err := os.ReadFile(stdoutPath); err != nil || string(got) != "ran" {
		t.Errorf("agent did not run as expected: stdout=%q err=%v", got, err)
	}
}

// TestRunContainer_FileWriteErrorAbortsBeforeAgent pins that a setup failure
// (spec.Files materialization, here) surfaces as a non-zero exit and never
// reaches the agent — mirroring RunInnerChild's own fail-fast contract.
func TestRunContainer_FileWriteErrorAbortsBeforeAgent(t *testing.T) {
	dir := t.TempDir()
	specPath := filepath.Join(dir, "spec.json")
	statePath := filepath.Join(dir, "state.json")

	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatalf("write blocker: %v", err)
	}

	spec := sandbox.Spec{
		ID:          "container-e2e-2",
		HarnessType: sandbox.HarnessShell,
		Argv:        []string{"/bin/sh", "-c", "echo should-not-run; exit 0"},
		WorkDir:     dir,
		Files: []sandbox.FileWrite{
			{Path: filepath.Join(blocker, "child.txt"), Content: "x"},
		},
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	exitCode, err := RunContainer(specPath, statePath)
	if err == nil {
		t.Fatal("expected an error when spec.Files cannot be materialized")
	}
	if exitCode != 1 {
		t.Errorf("exitCode = %d, want 1", exitCode)
	}
}

// TestRunContainer_HomeSkeletonFailureAbortsBeforeAgent is the wiring-seam
// guard for PR6 of docs/plans/workspace-home-volume-persistence.md: the
// skeleton check is a function RunContainer has to CALL, and
// home_skeleton_linux_test.go — which calls verifyHomeSkeleton directly —
// would stay green if the call site were deleted.
//
// That is not a hypothetical: what the check defends against produces no error
// of its own (a uid-0 ~/.claude just makes the harness silently fail to persist
// its credentials), so an unwired check and a working one look identical from
// every other angle. Hence both halves of the assertion: the run fails, AND the
// agent never ran.
func TestRunContainer_HomeSkeletonFailureAbortsBeforeAgent(t *testing.T) {
	dir := t.TempDir()
	stdoutPath := filepath.Join(dir, "stdout.txt")
	specPath := filepath.Join(dir, "spec.json")
	statePath := filepath.Join(dir, "state.json")

	spec := sandbox.Spec{
		ID:          "container-home-skeleton-1",
		HarnessType: sandbox.HarnessShell,
		Argv:        []string{"/bin/sh", "-c", "printf ran"},
		WorkDir:     dir,
		Env:         map[string]string{"PATH": "/usr/bin:/bin"},
		// The directory the workspace home init is supposed to have created,
		// named the way BuildSandboxSpec names it: a root (the sandbox $HOME)
		// plus entries relative to it. Absent here, which is what a job of this
		// workspace deleting ~/.claude leaves behind.
		HomeSkeletonRoot:  filepath.Join(dir, "home"),
		HomeSkeletonDirs:  []string{".claude"},
		StdoutCaptureFile: stdoutPath,
		Foreground:        true,
	}
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		t.Fatalf("write spec: %v", err)
	}

	exitCode, err := RunContainer(specPath, statePath)
	if err == nil {
		t.Fatal("RunContainer succeeded with a workspace HOME skeleton directory missing")
	}
	if exitCode != 1 {
		t.Errorf("exitCode = %d, want 1", exitCode)
	}
	if !strings.Contains(err.Error(), "workspace HOME skeleton") {
		t.Errorf("error is not the skeleton check's:\n%v", err)
	}
	// ...and specifically the MISSING-directory branch, not the "declared with
	// no root" wiring guard, which would also carry that prefix while proving
	// nothing about the check this case is here for.
	if !strings.Contains(err.Error(), "is missing") {
		t.Errorf("error is not the missing-directory branch:\n%v", err)
	}
	if _, statErr := os.Stat(stdoutPath); statErr == nil {
		t.Error("the agent ran despite the skeleton check failing; the check must abort before the harness starts")
	}
}
