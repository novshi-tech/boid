package sandbox_test

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestWriteSandboxScripts_GateRoleMustNotReferenceUnmountedProjectPath(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:        "phase1-gate-path",
		TaskID:       "task-gate-1",
		ProjectID:    "proj-1",
		ProjectDir:   "/tmp/project-gate",
		BoidBinary:   "/bin/true",
		BrokerSocket: "/run/boid/broker.sock",
		BrokerToken:  "token",
		Role:         "gate",
		HookScript:   "push-pr.sh",
		TaskJSON:     `{"id":"task-gate-1"}`,
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
	gatePath := cfg.ProjectDir + "/.boid/gates/" + cfg.HookScript

	if strings.Contains(inner, gatePath) &&
		!strings.Contains(setup, cfg.ProjectDir+"/.boid") &&
		!strings.Contains(setup, gatePath) {
		t.Fatalf("gate script references %q without mounting or copying it into the sandbox", gatePath)
	}
}

func TestWriteSandboxScripts_HookRoleMustQuotePathsAndPayload(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:        "phase1-quoting",
		ProjectID:    "proj-1",
		ProjectDir:   "/tmp/project with spaces",
		HomeDir:      "/tmp/home dir",
		HooksDir:     "/tmp/project with spaces/.boid/hooks",
		HookScript:   "review.sh",
		BoidBinary:   "/bin/true",
		BrokerSocket: "/run/boid/broker.sock",
		BrokerToken:  "token",
		Role:         "hook",
		PayloadJSON:  `{"text":"it's tricky"}`,
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
