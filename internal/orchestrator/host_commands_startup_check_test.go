package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestValidateHostCommandsInstalled(t *testing.T) {
	tmpDir := t.TempDir()
	existingAbs := filepath.Join(tmpDir, "some-tool")
	if err := os.WriteFile(existingAbs, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("seed existing abs-path command: %v", err)
	}
	missingAbs := filepath.Join(tmpDir, "does-not-exist")

	lookPath := func(name string) (string, error) {
		switch name {
		case "gh":
			return "/usr/bin/gh", nil
		case "atl":
			return "", fmt.Errorf("exec: %q: executable file not found in $PATH", name)
		}
		return "", fmt.Errorf("unexpected lookPath(%q)", name)
	}

	hostCommands := map[string]HostCommandSpec{
		"gh":            {},                          // empty Path -> lookPath; found
		"atl":           {},                          // empty Path -> lookPath; missing
		"present-abs":   {Path: existingAbs},         // absolute Path; exists
		"missing-abs":   {Path: missingAbs},          // absolute Path; missing
		"project-local": {Path: "./scripts/tool.sh"}, // relative Path; skipped entirely
	}

	missing := ValidateHostCommandsInstalled(hostCommands, lookPath)

	want := []string{"atl", "missing-abs"}
	if len(missing) != len(want) {
		t.Fatalf("missing = %v, want %v", missing, want)
	}
	for i := range want {
		if missing[i] != want[i] {
			t.Errorf("missing[%d] = %q, want %q (full: %v)", i, missing[i], want[i], missing)
		}
	}
}

func TestValidateHostCommandsInstalled_EmptyConfig(t *testing.T) {
	missing := ValidateHostCommandsInstalled(nil, func(string) (string, error) { return "", nil })
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
}

func TestValidateHostCommandsInstalled_AllResolved(t *testing.T) {
	hostCommands := map[string]HostCommandSpec{
		"gh": {},
	}
	missing := ValidateHostCommandsInstalled(hostCommands, func(string) (string, error) { return "/usr/bin/gh", nil })
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none (lookPath always succeeds)", missing)
	}
}
