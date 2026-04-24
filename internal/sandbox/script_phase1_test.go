package sandbox_test

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// Gate-like invocations use the script's original host path. The caller binds
// the kit root directory at the same path so relative source() works.
func TestPrepare_GateArgvUsesOriginalPath(t *testing.T) {
	const kitRoot = "/home/user/.local/share/boid/kits/git-auto-merge"
	gateArgv := []string{kitRoot + "/gates/push-pr.sh"}
	spec := sandbox.Spec{
		ID:      "phase1-gate-path",
		WorkDir: "/tmp/project-gate",
		Env:     map[string]string{"HOME": "/tmp"},
		Argv:    gateArgv,
		Mounts: []sandbox.Mount{
			{Target: "/tmp", Type: sandbox.MountTmpfs},
			{Target: "/tmp/project-gate", Type: sandbox.MountTmpfs},
			{Source: kitRoot, Target: kitRoot, Type: sandbox.MountBind, ReadOnly: true},
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
	if !strings.Contains(setup, "mount --bind "+kitRoot) {
		t.Fatalf("setup should bind-mount the kit root %q, got:\n%s", kitRoot, setup)
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
