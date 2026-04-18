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
// This test asserts the primitives round-trip through Prepare correctly.
func TestPrepare_GateArgvRequiresMatchingMount(t *testing.T) {
	gateArgv := []string{"/opt/boid/gates/push-pr.sh"}
	spec := sandbox.Spec{
		ID:      "phase1-gate-path",
		WorkDir: "/tmp/project-gate",
		Env:     map[string]string{"HOME": "/tmp"},
		Argv:    gateArgv,
		Mounts: []sandbox.Mount{
			{Target: "/tmp", Type: sandbox.MountTmpfs},
			{Target: "/tmp/project-gate", Type: sandbox.MountTmpfs},
			{Source: "/tmp/staged-gates/push-pr.sh", Target: "/opt/boid/gates/push-pr.sh", Type: sandbox.MountBind, IsFile: true},
		},
		StdinBytes: []byte(`{"id":"task-gate-1"}`),
	}

	outerPath, err := sandbox.Prepare(spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	prefix := strings.TrimSuffix(outerPath, "-outer.sh")
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
func TestPrepare_QuotesPathsAndPayload(t *testing.T) {
	projectDir := "/tmp/project with spaces"
	homeDir := "/tmp/home dir"
	spec := sandbox.Spec{
		ID:      "phase1-quoting",
		WorkDir: projectDir,
		Env:     map[string]string{"HOME": homeDir},
		Argv:    []string{projectDir + "/.boid/hooks/review.sh"},
		Mounts: []sandbox.Mount{
			{Source: projectDir, Target: projectDir, Type: sandbox.MountBind},
		},
		StdinBytes: []byte(`{"text":"it's tricky"}`),
	}

	outerPath, err := sandbox.Prepare(spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	prefix := strings.TrimSuffix(outerPath, "-outer.sh")
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

	if strings.Contains(inner, "\nexport HOME="+homeDir+"\n") {
		t.Errorf("HOME path with spaces is interpolated without shell quoting")
	}
	if strings.Contains(inner, "\ncd "+projectDir+"\n") {
		t.Errorf("working directory with spaces is interpolated without shell quoting")
	}
	if strings.Contains(setup, fmt.Sprintf("mount --bind %s ", projectDir)) {
		t.Errorf("setup script interpolates bind mount source without shell quoting")
	}

	cmd := exec.Command("bash", "-n", innerPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("inner script should remain syntactically valid for quoted payloads: %v\n%s", err, out)
	}
}
