package orchestrator

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkspaceMeta_YAMLRoundTrip(t *testing.T) {
	t.Parallel()

	original := WorkspaceMeta{
		Kits: []string{"go-tools", "node-lts"},
		Env: map[string]string{
			"GOPATH": "/home/user/go",
			"DEBUG":  "1",
		},
	}

	data, err := yaml.Marshal(&original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded WorkspaceMeta
	if err := yaml.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	// Check Kits
	if len(decoded.Kits) != len(original.Kits) {
		t.Errorf("Kits length: got %d, want %d", len(decoded.Kits), len(original.Kits))
	}
	for i, k := range original.Kits {
		if i < len(decoded.Kits) && decoded.Kits[i] != k {
			t.Errorf("Kits[%d]: got %q, want %q", i, decoded.Kits[i], k)
		}
	}

	// Check Env
	for key, val := range original.Env {
		if got := decoded.Env[key]; got != val {
			t.Errorf("Env[%q]: got %q, want %q", key, got, val)
		}
	}
}

func TestWorkspaceMeta_EmptyOmitsFields(t *testing.T) {
	t.Parallel()

	meta := WorkspaceMeta{}
	data, err := yaml.Marshal(&meta)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// An empty WorkspaceMeta should marshal to "{}\n" (all fields omitted).
	s := string(data)
	if s != "{}\n" {
		t.Errorf("expected empty yaml to be \"{}\", got: %q", s)
	}
}

func TestWorkspaceMeta_DockerCapabilityRoundTrip(t *testing.T) {
	t.Parallel()

	const input = `
capabilities:
  docker: {}
`
	var meta WorkspaceMeta
	if err := yaml.Unmarshal([]byte(input), &meta); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if meta.Capabilities.Docker == nil {
		t.Fatal("Capabilities.Docker is nil, want non-nil (docker enabled)")
	}

	// Round-trip: marshal back and re-parse
	data, err := yaml.Marshal(&meta)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var meta2 WorkspaceMeta
	if err := yaml.Unmarshal(data, &meta2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if meta2.Capabilities.Docker == nil {
		t.Fatal("round-trip: Capabilities.Docker is nil, want non-nil")
	}
}
