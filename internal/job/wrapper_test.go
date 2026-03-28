package job_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/job"
)

func TestWriteSandboxScripts(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-job-001",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		Env: map[string]string{
			"MY_VAR": "hello",
		},
		HostCommands: []string{"git", "gh"},
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-job-001"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"

	// Clean up generated scripts after test
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	// Verify outer script
	outerContent, err := os.ReadFile(outerPath)
	if err != nil {
		t.Fatalf("read outer script: %v", err)
	}
	outer := string(outerContent)
	if !strings.Contains(outer, "pasta --config-net") {
		t.Error("outer script missing 'pasta --config-net'")
	}
	if !strings.Contains(outer, "unshare --mount") {
		t.Error("outer script missing 'unshare --mount'")
	}

	// Verify setup script
	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)
	if !strings.Contains(setup, "chroot") {
		t.Error("setup script missing 'chroot'")
	}
	if !strings.Contains(setup, cfg.ProjectDir) {
		t.Errorf("setup script missing project dir %q", cfg.ProjectDir)
	}
	// Verify shim symlinks for host commands
	if !strings.Contains(setup, `ln -sf boid "$ROOT/opt/boid/bin/git"`) {
		t.Error("setup script missing git shim symlink")
	}
	if !strings.Contains(setup, `ln -sf boid "$ROOT/opt/boid/bin/gh"`) {
		t.Error("setup script missing gh shim symlink")
	}

	// Verify inner script
	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	inner := string(innerContent)
	if !strings.Contains(inner, "boid job done test-job-001") {
		t.Error("inner script missing 'boid job done {jobID}'")
	}
	if !strings.Contains(inner, "/workspace/.boid/hooks/run-agent.sh") {
		t.Error("inner script missing hook invocation")
	}
	if strings.Contains(inner, "exec /workspace/.boid/hooks/run-agent.sh") {
		t.Error("inner script must not use exec (would skip EXIT trap)")
	}
	if !strings.Contains(inner, "BOID_SOCKET=/run/boid/server.sock") {
		t.Error("inner script missing BOID_SOCKET")
	}
	if !strings.Contains(inner, `MY_VAR="hello"`) {
		t.Error("inner script missing env var MY_VAR")
	}
}

func TestWriteSandboxScripts_AdditionalBindings(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-job-bind",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-build.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		AdditionalBindings: []string{
			"/home/user/.local/bin",
			"/home/user/.local/share/go",
			"/home/user/go",
		},
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-job-bind"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	// Verify setup script mounts additional bindings
	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)

	for _, binding := range cfg.AdditionalBindings {
		if !strings.Contains(setup, fmt.Sprintf("mount --bind %s \"$ROOT%s\"", binding, binding)) {
			t.Errorf("setup script missing bind mount for %s", binding)
		}
		if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s\"", binding)) {
			t.Errorf("setup script missing read-only remount for %s", binding)
		}
	}

	// Verify inner script PATH includes additional binding paths
	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	inner := string(innerContent)

	// /home/user/.local/bin ends with /bin → added directly
	if !strings.Contains(inner, "/home/user/.local/bin") {
		t.Error("inner script PATH missing /home/user/.local/bin")
	}
	// /home/user/.local/share/go → /home/user/.local/share/go/bin
	if !strings.Contains(inner, "/home/user/.local/share/go/bin") {
		t.Error("inner script PATH missing /home/user/.local/share/go/bin")
	}
	// /home/user/go → /home/user/go/bin
	if !strings.Contains(inner, "/home/user/go/bin") {
		t.Error("inner script PATH missing /home/user/go/bin")
	}
}
