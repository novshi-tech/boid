package dispatcher_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestBuildCommandJobSpec_BasicFields(t *testing.T) {
	input := dispatcher.CommandJobInput{
		ProjectID:      "proj-1",
		ProjectWorkDir: "/work/proj",
		Argv:           []string{"bash", "-c", "echo hello"},
		Env:            map[string]string{"FOO": "bar"},
		Readonly:       false,
	}

	spec := dispatcher.BuildCommandJobSpec(input)

	if spec.ProjectID != "proj-1" {
		t.Errorf("ProjectID = %q, want %q", spec.ProjectID, "proj-1")
	}
	if spec.HandlerID != "" {
		t.Errorf("HandlerID = %q, want empty", spec.HandlerID)
	}
	if spec.Kind != orchestrator.JobKindExec {
		t.Errorf("Kind = %q, want %q", spec.Kind, orchestrator.JobKindExec)
	}
	if len(spec.Argv) != 3 || spec.Argv[0] != "bash" {
		t.Errorf("Argv = %v, want [bash -c echo hello]", spec.Argv)
	}
	if spec.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] = %q, want %q", spec.Env["FOO"], "bar")
	}
}

func TestBuildCommandJobSpec_Visibility(t *testing.T) {
	bindings := []orchestrator.BindMount{{Source: "/tools/bin", Target: "/tools/bin"}}

	// writable
	spec := dispatcher.BuildCommandJobSpec(dispatcher.CommandJobInput{
		ProjectID:          "p",
		ProjectWorkDir:     "/work",
		Argv:               []string{"bash"},
		AdditionalBindings: bindings,
		Readonly:           false,
	})
	if !spec.Visibility.Writable {
		t.Error("Writable should be true when Readonly=false")
	}
	if spec.Visibility.UseWorktree {
		t.Error("UseWorktree should always be false")
	}
	if spec.Visibility.ProjectDir != "/work" {
		t.Errorf("ProjectDir = %q, want /work", spec.Visibility.ProjectDir)
	}
	if len(spec.Visibility.AdditionalBindings) != 1 {
		t.Errorf("AdditionalBindings len = %d, want 1", len(spec.Visibility.AdditionalBindings))
	}

	// readonly
	spec2 := dispatcher.BuildCommandJobSpec(dispatcher.CommandJobInput{
		ProjectID:      "p",
		ProjectWorkDir: "/work",
		Argv:           []string{"bash"},
		Readonly:       true,
	})
	if spec2.Visibility.Writable {
		t.Error("Writable should be false when Readonly=true")
	}
}

func TestBuildCommandJobSpec_BuiltinPolicies(t *testing.T) {
	spec := dispatcher.BuildCommandJobSpec(dispatcher.CommandJobInput{
		ProjectID:      "p",
		ProjectWorkDir: "/work",
		Argv:           []string{"bash"},
	})

	if _, ok := spec.BuiltinPolicies["boid"]; !ok {
		t.Error("BuiltinPolicies should contain 'boid'")
	}
	if _, ok := spec.BuiltinPolicies["git"]; !ok {
		t.Error("BuiltinPolicies should contain 'git'")
	}
}

func TestBuildCommandJobSpec_HostCommands(t *testing.T) {
	hostCmds := map[string]orchestrator.HostCommandSpec{
		"curl": {Allow: []string{"curl https://*"}},
	}
	spec := dispatcher.BuildCommandJobSpec(dispatcher.CommandJobInput{
		ProjectID:      "p",
		ProjectWorkDir: "/work",
		Argv:           []string{"bash"},
		HostCommands:   hostCmds,
	})

	if _, ok := spec.HostCommands["curl"]; !ok {
		t.Error("HostCommands should contain 'curl'")
	}
}

func TestBuildCommandJobSpec_Interactive(t *testing.T) {
	// CLI path: no terminal attached — Interactive=false
	cliNonTTY := dispatcher.BuildCommandJobSpec(dispatcher.CommandJobInput{
		ProjectID:      "p",
		ProjectWorkDir: "/work",
		Argv:           []string{"bash"},
		Interactive:    false,
	})
	if cliNonTTY.Interactive {
		t.Error("CLI path (no terminal): Interactive should be false")
	}

	// CLI path: terminal attached — Interactive=true
	cliTTY := dispatcher.BuildCommandJobSpec(dispatcher.CommandJobInput{
		ProjectID:      "p",
		ProjectWorkDir: "/work",
		Argv:           []string{"bash"},
		Interactive:    true,
	})
	if !cliTTY.Interactive {
		t.Error("CLI path (terminal): Interactive should be true")
	}

	// Daemon path: Interactive=true (Web UI always wants PTY)
	daemonSpec := dispatcher.BuildCommandJobSpec(dispatcher.CommandJobInput{
		ProjectID:      "p",
		ProjectWorkDir: "/work",
		Argv:           []string{"bash"},
		Interactive:    true,
	})
	if !daemonSpec.Interactive {
		t.Error("Daemon path: Interactive should be true")
	}
}

func TestBuildCommandJobSpec_CLIAndDaemonSameJobSpec(t *testing.T) {
	// Verifies that CLI and daemon paths produce identical JobSpec except for
	// the Interactive flag, which the CLI sets based on terminal detection at
	// entry time.
	input := dispatcher.CommandJobInput{
		ProjectID:      "proj",
		ProjectWorkDir: "/repo",
		Argv:           []string{"bash", "-c", "make test"},
		Env:            map[string]string{"CI": "true"},
		HostCommands: map[string]orchestrator.HostCommandSpec{
			"make": {},
		},
		Readonly: true,
	}

	cliInput := input
	cliInput.Interactive = false

	daemonInput := input
	daemonInput.Interactive = true

	cliSpec := dispatcher.BuildCommandJobSpec(cliInput)
	daemonSpec := dispatcher.BuildCommandJobSpec(daemonInput)

	// Structural fields must match
	if cliSpec.ProjectID != daemonSpec.ProjectID {
		t.Errorf("ProjectID mismatch: %q vs %q", cliSpec.ProjectID, daemonSpec.ProjectID)
	}
	if cliSpec.Kind != daemonSpec.Kind {
		t.Errorf("Kind mismatch: %q vs %q", cliSpec.Kind, daemonSpec.Kind)
	}
	if cliSpec.Visibility.Writable != daemonSpec.Visibility.Writable {
		t.Errorf("Visibility.Writable mismatch: %v vs %v", cliSpec.Visibility.Writable, daemonSpec.Visibility.Writable)
	}
	if len(cliSpec.HostCommands) != len(daemonSpec.HostCommands) {
		t.Errorf("HostCommands len mismatch: %d vs %d", len(cliSpec.HostCommands), len(daemonSpec.HostCommands))
	}

	// Only Interactive differs
	if cliSpec.Interactive == daemonSpec.Interactive {
		t.Error("Interactive flag should differ between CLI and daemon paths")
	}
}
