package sandbox_test

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// Gate-like invocations run an argv that points into /opt/boid/gates/. The
// caller is responsible for adding a bind-mount that makes that path resolve.
// This test asserts sandbox does not silently succeed if the gate argv is
// referenced without a matching staging mount.
func TestWriteSandboxScripts_GateArgvRequiresMatchingMount(t *testing.T) {
	gateArgv := []string{"/opt/boid/gates/push-pr.sh"}
	cfg := sandbox.WrapperConfig{
		JobID:      "phase1-gate-path",
		TaskID:     "task-gate-1",
		ProjectID:  "proj-1",
		ProjectDir: "/tmp/project-gate",
		HomeDir:    "/tmp",
		BoidBinary: "/bin/true",
		Argv:       gateArgv,
		AdditionalBindings: []sandbox.BindMount{
			{Source: "/tmp/staged-gates/push-pr.sh", Target: "/opt/boid/gates/push-pr.sh", IsFile: true},
		},
		StdinBytes: []byte(`{"id":"task-gate-1"}`),
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-phase1-gate-path"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		_ = os.Remove(outerPath)
		_ = os.Remove(setupPath)
		_ = os.Remove(innerPath)
	})

	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}

	inner := string(innerContent)
	setup := string(setupContent)

	if !strings.Contains(inner, gateArgv[0]) {
		t.Fatalf("inner script should invoke %q", gateArgv[0])
	}
	if !strings.Contains(setup, "mount --bind /tmp/staged-gates/push-pr.sh") {
		t.Fatalf("setup should bind-mount the staged gate script into /opt/boid/gates/")
	}
}

// Quoting regression: paths with spaces and payloads with single quotes must
// be rendered safely by the sandbox script generator.
func TestWriteSandboxScripts_QuotesPathsAndPayload(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "phase1-quoting",
		ProjectID:       "proj-1",
		ProjectDir:      "/tmp/project with spaces",
		HomeDir:         "/tmp/home dir",
		BoidBinary:      "/bin/true",
		MountProjectDir: true,
		Argv:            []string{"/tmp/project with spaces/.boid/hooks/review.sh"},
		StdinBytes:      []byte(`{"text":"it's tricky"}`),
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-phase1-quoting"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		_ = os.Remove(outerPath)
		_ = os.Remove(setupPath)
		_ = os.Remove(innerPath)
	})

	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}

	inner := string(innerContent)
	setup := string(setupContent)

	if strings.Contains(inner, "\nexport HOME="+cfg.HomeDir+"\n") {
		t.Errorf("HOME path with spaces is interpolated without shell quoting")
	}
	if strings.Contains(inner, "\ncd "+cfg.ProjectDir+"\n") {
		t.Errorf("working directory with spaces is interpolated without shell quoting")
	}
	if strings.Contains(setup, fmt.Sprintf("mount --bind %s ", cfg.ProjectDir)) {
		t.Errorf("setup script interpolates bind mount source without shell quoting")
	}

	cmd := exec.Command("bash", "-n", innerPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("inner script should remain syntactically valid for quoted payloads: %v\n%s", err, out)
	}
}
