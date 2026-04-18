package sandbox

import (
	"os"
	"strings"
	"testing"
)

func TestBoidBinaryRendersAsBindMount(t *testing.T) {
	cfg := WrapperConfig{
		JobID:      "m2-check-001",
		ProjectID:  "p",
		ProjectDir: "/tmp/p",
		BoidBinary: "/usr/local/bin/boid",
		Argv:       []string{"/bin/true"},
	}
	outerPath, err := WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	setupPath := strings.TrimSuffix(outerPath, "-outer.sh") + "-setup.sh"
	innerPath := strings.TrimSuffix(outerPath, "-outer.sh") + "-inner.sh"
	defer os.Remove(outerPath)
	defer os.Remove(setupPath)
	defer os.Remove(innerPath)

	content, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(content)
	// binary must be bind-mounted as a single file (touch + mount --bind + ro remount)
	mustContain := []string{
		`touch "$ROOT/opt/boid/bin/boid"`,
		`mount --bind /usr/local/bin/boid "$ROOT/opt/boid/bin/boid"`,
		`mount -o remount,bind,ro "$ROOT/opt/boid/bin/boid"`,
	}
	for _, s := range mustContain {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in setup:\n%s", s, got)
		}
	}
	// must NOT contain a cp of the boid binary
	if strings.Contains(got, "cp /usr/local/bin/boid") {
		t.Errorf("unexpected cp of boid binary: setup uses copy, not bind-mount:\n%s", got)
	}
}
