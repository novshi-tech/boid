package dispatcher

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestWriteHostGateWrapper_ContractMatchesSandboxedGate(t *testing.T) {
	worktree := t.TempDir()
	script := worktree + "/scripts/auto-merge.sh"

	taskJSON, _ := json.Marshal(map[string]any{"id": "task-1"})
	spec := &orchestrator.JobSpec{
		TaskID:       "task-1",
		ProjectID:    "proj-1",
		HandlerID:    "git-auto-merge/auto-merge",
		Argv:         []string{script},
		PrimaryInput: taskJSON,
		Env: map[string]string{
			"BOID_BASE_BRANCH": "feature/BGO-170",
			"KIT_VAR":          "kit-value",
		},
	}

	wrapperPath, outputPath, err := writeHostGateWrapper("job-1", worktree, spec, "/usr/local/bin/boid")
	if err != nil {
		t.Fatalf("writeHostGateWrapper: %v", err)
	}
	t.Cleanup(func() { removeHostGateArtifacts(wrapperPath, outputPath) })

	got, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("read wrapper: %v", err)
	}
	wrapper := string(got)

	wantSubstrings := []string{
		"cd '" + worktree + "'",
		"export BOID_BASE_BRANCH='feature/BGO-170'",
		"export BOID_JOB_ID='job-1'",
		"export BOID_TASK_ID='task-1'",
		"export KIT_VAR='kit-value'",
		"OUTPUT_FILE='" + outputPath + "'",
		`BOID_BIN='/usr/local/bin/boid'`,
		`PAYLOAD_FILE="$HOME/.boid/output/payload_patch.yaml"`,
		`_boid_done()`,
		`if [ -f "$PAYLOAD_FILE" ]`,
		`trap '_exit=$?; _boid_done "$_exit"' EXIT`,
		"printf '%s' '" + string(taskJSON) + "' | '" + script + "' > \"$OUTPUT_FILE\"",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(wrapper, want) {
			t.Errorf("wrapper missing %q\n--- wrapper ---\n%s", want, wrapper)
		}
	}

	// File mode should be executable.
	info, err := os.Stat(wrapperPath)
	if err != nil {
		t.Fatalf("stat wrapper: %v", err)
	}
	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("wrapper mode = %v, want owner-executable", info.Mode().Perm())
	}
}

// ensureHostGateWorktree falls back to os.TempDir() when the runner has no
// worktree manager and no project lookup configured (e.g. non-git projects).
func TestEnsureHostGateWorktree_FallbackWhenNoWorktree(t *testing.T) {
	r := &Runner{
		Worktrees: nil,
		Projects:  nil,
	}
	spec := &orchestrator.JobSpec{
		TaskID:    "task-1",
		ProjectID: "proj-1",
	}
	got, err := r.ensureHostGateWorktree(spec, "")
	if err != nil {
		t.Fatalf("ensureHostGateWorktree: %v", err)
	}
	if got == "" {
		t.Fatal("expected non-empty fallback path")
	}
	// With no worktrees/projects, should fall back to os.TempDir().
	if got != os.TempDir() {
		t.Errorf("got %q, want %q (os.TempDir)", got, os.TempDir())
	}
}

// ensureHostGateWorktree returns currentPath immediately when already resolved.
func TestEnsureHostGateWorktree_ReturnsCurrentPathWhenSet(t *testing.T) {
	r := &Runner{}
	spec := &orchestrator.JobSpec{TaskID: "task-1"}
	want := "/some/worktree/path"
	got, err := r.ensureHostGateWorktree(spec, want)
	if err != nil {
		t.Fatalf("ensureHostGateWorktree: %v", err)
	}
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// When the gate has no PrimaryInput (rare but supported), stdin is closed via
// /dev/null so the script doesn't block waiting for input.
func TestWriteHostGateWrapper_EmptyStdinUsesDevNull(t *testing.T) {
	worktree := t.TempDir()
	spec := &orchestrator.JobSpec{
		TaskID: "task-1",
		Argv:   []string{worktree + "/g.sh"},
	}

	wrapperPath, outputPath, err := writeHostGateWrapper("job-1", worktree, spec, "/usr/local/bin/boid")
	if err != nil {
		t.Fatalf("writeHostGateWrapper: %v", err)
	}
	t.Cleanup(func() { removeHostGateArtifacts(wrapperPath, outputPath) })

	got, err := os.ReadFile(wrapperPath)
	if err != nil {
		t.Fatalf("read wrapper: %v", err)
	}
	if !strings.Contains(string(got), "< /dev/null") {
		t.Errorf("wrapper missing '< /dev/null' guard for empty stdin\n%s", string(got))
	}
	if strings.Contains(string(got), "printf '%s' ''") {
		t.Errorf("wrapper should not pipe empty string when PrimaryInput is nil\n%s", string(got))
	}
}
