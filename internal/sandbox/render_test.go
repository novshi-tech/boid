package sandbox

import (
	"os"
	"strings"
	"testing"
)

// Bind-mount rendering for host files (e.g. boid binary, sockets, gate scripts).
// Caller constructs the Mount entry with IsFile+ReadOnly; setup script must
// touch the target path before binding and remount read-only afterward.
func TestPrepare_FileBindMountRendering(t *testing.T) {
	spec := Spec{
		ID:      "m4-bind-file",
		WorkDir: "/tmp/p",
		Env:     map[string]string{"HOME": "/tmp/p"},
		Argv:    []string{"/bin/true"},
		Mounts: []Mount{
			{Source: "/usr/local/bin/boid", Target: "/opt/boid/bin/boid", Type: MountBind, IsFile: true, ReadOnly: true},
		},
	}
	outerPath, err := Prepare(spec)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
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

	must := []string{
		`touch "$ROOT/opt/boid/bin/boid"`,
		`mount --bind /usr/local/bin/boid "$ROOT/opt/boid/bin/boid"`,
		`mount -o remount,bind,ro "$ROOT/opt/boid/bin/boid"`,
	}
	for _, s := range must {
		if !strings.Contains(got, s) {
			t.Errorf("missing %q in setup:\n%s", s, got)
		}
	}
	if strings.Contains(got, "cp /usr/local/bin/boid") {
		t.Errorf("unexpected cp: file bind-mounts should not copy content\n%s", got)
	}
}
