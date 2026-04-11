package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClaudeCodeHook_NonInteractiveStreamsForAttach(t *testing.T) {
	kitDir := os.Getenv("BOID_KITS_DIR")
	if kitDir == "" {
		kitDir = filepath.Join(os.Getenv("HOME"), ".local", "share", "boid", "kits",
			"github.com", "novshi-tech", "boid-kits")
	}
	hookPath := filepath.Join(kitDir, "claude-code", "hooks", "run-agent.sh")
	data, err := os.ReadFile(hookPath)
	if err != nil {
		t.Skipf("kit hook not available: %v", err)
	}

	script := string(data)
	for _, want := range []string{
		"--verbose",
		"--output-format=stream-json",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("claude hook missing %q:\n%s", want, script)
		}
	}
}
