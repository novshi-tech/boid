package sandbox

import (
	"slices"
	"testing"
)

func TestHostCommandEnv_StripsBoidPrefixedEnv(t *testing.T) {
	t.Setenv("BOID_DAEMON_CHILD", "1")
	t.Setenv("BOID_BROKER_SOCKET", "/run/user/1000/boid-broker.sock")
	t.Setenv("BOID_BROKER_TOKEN", "secret")
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("HOME", "/home/user")

	got := hostCommandEnv(nil)

	for _, kv := range got {
		if len(kv) > 5 && kv[:5] == "BOID_" {
			t.Errorf("hostCommandEnv leaked %q to child", kv)
		}
	}
	if !slices.Contains(got, "PATH=/usr/bin:/bin") {
		t.Errorf("hostCommandEnv dropped PATH; got %v", got)
	}
	if !slices.Contains(got, "HOME=/home/user") {
		t.Errorf("hostCommandEnv dropped HOME; got %v", got)
	}
}

func TestHostCommandEnv_DefEnvOverlays(t *testing.T) {
	t.Setenv("BOID_DAEMON_CHILD", "1")
	t.Setenv("PATH", "/usr/bin")

	got := hostCommandEnv(map[string]string{
		"FOO":   "bar",
		"PATH":  "/custom/bin",
	})

	// def.Env entries are appended; later entries take precedence in os/exec.
	if !slices.Contains(got, "FOO=bar") {
		t.Errorf("def.Env FOO not present; got %v", got)
	}
	// Both PATH= entries are in the slice (overlay is by append-and-let-exec-take-last).
	pathCount := 0
	for _, kv := range got {
		if len(kv) >= 5 && kv[:5] == "PATH=" {
			pathCount++
		}
	}
	if pathCount < 2 {
		t.Errorf("expected def.Env PATH overlay to be appended; got %d PATH= entries", pathCount)
	}
}

func TestHostCommandEnv_NilDefEnvOK(t *testing.T) {
	t.Setenv("PATH", "/usr/bin")
	got := hostCommandEnv(nil)
	if len(got) == 0 {
		t.Fatalf("hostCommandEnv returned empty slice")
	}
}
