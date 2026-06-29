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

func TestWorkspaceMeta_AllowedDomainsRoundTrip(t *testing.T) {
	t.Parallel()

	const input = `
allowed_domains:
  - .cosmos.azure.com
  - api.openai.com
`
	var meta WorkspaceMeta
	if err := yaml.Unmarshal([]byte(input), &meta); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got, want := len(meta.AllowedDomains), 2; got != want {
		t.Fatalf("AllowedDomains length = %d, want %d", got, want)
	}
	if meta.AllowedDomains[0] != ".cosmos.azure.com" || meta.AllowedDomains[1] != "api.openai.com" {
		t.Errorf("AllowedDomains = %v", meta.AllowedDomains)
	}

	// Re-marshal and reparse to ensure stability.
	data, err := yaml.Marshal(&meta)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var meta2 WorkspaceMeta
	if err := yaml.Unmarshal(data, &meta2); err != nil {
		t.Fatalf("Unmarshal round-trip: %v", err)
	}
	if len(meta2.AllowedDomains) != 2 {
		t.Errorf("round-trip lost AllowedDomains: %v", meta2.AllowedDomains)
	}
}

func TestResolveAllowedDomains(t *testing.T) {
	t.Parallel()

	floor := []string{"pypi.org", "github.com", ".docker.io"}

	t.Run("nil workspace returns floor", func(t *testing.T) {
		got := ResolveAllowedDomains(floor, nil)
		if !equalStringSlice(got, floor) {
			t.Errorf("got %v, want %v", got, floor)
		}
	})

	t.Run("empty workspace returns floor", func(t *testing.T) {
		got := ResolveAllowedDomains(floor, &WorkspaceMeta{})
		if !equalStringSlice(got, floor) {
			t.Errorf("got %v, want %v", got, floor)
		}
	})

	t.Run("workspace adds entries on top of floor", func(t *testing.T) {
		ws := &WorkspaceMeta{AllowedDomains: []string{".cosmos.azure.com", "api.openai.com"}}
		got := ResolveAllowedDomains(floor, ws)
		want := []string{"pypi.org", "github.com", ".docker.io", ".cosmos.azure.com", "api.openai.com"}
		if !equalStringSlice(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("workspace duplicates of floor are dropped", func(t *testing.T) {
		// Case-insensitive: PYPI.ORG should be recognized as the same as pypi.org.
		ws := &WorkspaceMeta{AllowedDomains: []string{"PYPI.ORG", "api.openai.com"}}
		got := ResolveAllowedDomains(floor, ws)
		want := []string{"pypi.org", "github.com", ".docker.io", "api.openai.com"}
		if !equalStringSlice(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("workspace cannot remove floor entries", func(t *testing.T) {
		// Even if a workspace declares no domains, the floor is preserved.
		ws := &WorkspaceMeta{AllowedDomains: nil}
		got := ResolveAllowedDomains(floor, ws)
		if !equalStringSlice(got, floor) {
			t.Errorf("got %v, want %v (floor preserved)", got, floor)
		}
	})

	t.Run("blank and whitespace entries are skipped", func(t *testing.T) {
		ws := &WorkspaceMeta{AllowedDomains: []string{"", "  ", "example.com"}}
		got := ResolveAllowedDomains(nil, ws)
		want := []string{"example.com"}
		if !equalStringSlice(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("nil floor and nil workspace returns empty", func(t *testing.T) {
		got := ResolveAllowedDomains(nil, nil)
		if len(got) != 0 {
			t.Errorf("got %v, want empty", got)
		}
	})
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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
