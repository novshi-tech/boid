//go:build linux

package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
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
