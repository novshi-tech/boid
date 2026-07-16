package orchestrator

import (
	"encoding/json"
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

func TestWorkspaceMeta_ExtraReposRoundTrip(t *testing.T) {
	t.Parallel()

	const input = `
extra_repos:
  - https://github.com/example/private-lib.git
  - git@bitbucket.org:example/other-lib.git
`
	var meta WorkspaceMeta
	if err := yaml.Unmarshal([]byte(input), &meta); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got, want := len(meta.ExtraRepos), 2; got != want {
		t.Fatalf("ExtraRepos length = %d, want %d", got, want)
	}
	if meta.ExtraRepos[0] != "https://github.com/example/private-lib.git" ||
		meta.ExtraRepos[1] != "git@bitbucket.org:example/other-lib.git" {
		t.Errorf("ExtraRepos = %v", meta.ExtraRepos)
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
	if len(meta2.ExtraRepos) != 2 {
		t.Errorf("round-trip lost ExtraRepos: %v", meta2.ExtraRepos)
	}
}

func TestWorkspaceMeta_ExtraReposOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	meta := WorkspaceMeta{}
	data, err := yaml.Marshal(&meta)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if s := string(data); s != "{}\n" {
		t.Errorf("expected empty yaml to omit extra_repos, got: %q", s)
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

// TestWorkspaceMeta_HostCommandsRoundTrip covers docs/plans/workspace-db-consolidation.md
// PR3's WorkspaceMeta.HostCommands field ([]string reference names) round
// trips through both YAML (workspace.yaml on-disk shape) and JSON (DB /
// API wire shape).
func TestWorkspaceMeta_HostCommandsRoundTrip(t *testing.T) {
	t.Parallel()

	original := WorkspaceMeta{HostCommands: []string{"gh", "aws"}}

	yamlData, err := yaml.Marshal(&original)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var yamlDecoded WorkspaceMeta
	if err := yaml.Unmarshal(yamlData, &yamlDecoded); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if !equalStringSlice(yamlDecoded.HostCommands, original.HostCommands) {
		t.Errorf("yaml round-trip: HostCommands = %v, want %v", yamlDecoded.HostCommands, original.HostCommands)
	}

	jsonData, err := json.Marshal(&original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var jsonDecoded WorkspaceMeta
	if err := json.Unmarshal(jsonData, &jsonDecoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if !equalStringSlice(jsonDecoded.HostCommands, original.HostCommands) {
		t.Errorf("json round-trip: HostCommands = %v, want %v", jsonDecoded.HostCommands, original.HostCommands)
	}
}

// TestWorkspaceMeta_ContainerImageRoundTrip covers the Phase 6-reserved,
// currently-inert ContainerImage field.
func TestWorkspaceMeta_ContainerImageRoundTrip(t *testing.T) {
	t.Parallel()

	original := WorkspaceMeta{ContainerImage: "ghcr.io/example/image:latest"}

	yamlData, err := yaml.Marshal(&original)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var yamlDecoded WorkspaceMeta
	if err := yaml.Unmarshal(yamlData, &yamlDecoded); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if yamlDecoded.ContainerImage != original.ContainerImage {
		t.Errorf("yaml round-trip: ContainerImage = %q, want %q", yamlDecoded.ContainerImage, original.ContainerImage)
	}

	jsonData, err := json.Marshal(&original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var jsonDecoded WorkspaceMeta
	if err := json.Unmarshal(jsonData, &jsonDecoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if jsonDecoded.ContainerImage != original.ContainerImage {
		t.Errorf("json round-trip: ContainerImage = %q, want %q", jsonDecoded.ContainerImage, original.ContainerImage)
	}
}

// TestWorkspaceMeta_AdditionalBindingsRoundTrip covers the workspace-level
// AdditionalBindings vestige field (decision 4: retained until Phase 4).
func TestWorkspaceMeta_AdditionalBindingsRoundTrip(t *testing.T) {
	t.Parallel()

	original := WorkspaceMeta{
		AdditionalBindings: []BindMount{
			{Source: "/opt/volta", Target: "/opt/volta", Mode: "rw"},
		},
	}

	yamlData, err := yaml.Marshal(&original)
	if err != nil {
		t.Fatalf("yaml.Marshal: %v", err)
	}
	var yamlDecoded WorkspaceMeta
	if err := yaml.Unmarshal(yamlData, &yamlDecoded); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	if len(yamlDecoded.AdditionalBindings) != 1 || yamlDecoded.AdditionalBindings[0] != original.AdditionalBindings[0] {
		t.Errorf("yaml round-trip: AdditionalBindings = %+v, want %+v", yamlDecoded.AdditionalBindings, original.AdditionalBindings)
	}

	jsonData, err := json.Marshal(&original)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var jsonDecoded WorkspaceMeta
	if err := json.Unmarshal(jsonData, &jsonDecoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(jsonDecoded.AdditionalBindings) != 1 || jsonDecoded.AdditionalBindings[0] != original.AdditionalBindings[0] {
		t.Errorf("json round-trip: AdditionalBindings = %+v, want %+v", jsonDecoded.AdditionalBindings, original.AdditionalBindings)
	}
}

// TestWorkspaceMeta_NewFieldsOmittedWhenEmpty extends
// TestWorkspaceMeta_EmptyOmitsFields to the three PR3 fields: an empty
// WorkspaceMeta must still marshal to "{}" in YAML with none of
// HostCommands/ContainerImage/AdditionalBindings appearing.
func TestWorkspaceMeta_NewFieldsOmittedWhenEmpty(t *testing.T) {
	t.Parallel()

	meta := WorkspaceMeta{}
	data, err := yaml.Marshal(&meta)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if s := string(data); s != "{}\n" {
		t.Errorf("expected empty yaml to omit new PR3 fields, got: %q", s)
	}
}

// TestGetWithWorkspace_DoesNotMutateWorkspaceMetaInPlace pins MAJOR 1's
// clone-before-expand contract: expandWorkspaceRuntimeForDispatch (called by
// ProjectStore.GetWithWorkspace right after WorkspaceStore.Load, see
// project_store.go) must not mutate the *WorkspaceMeta it is handed in
// place. DB/yaml-stored values must stay raw (secret-leak / TOCTOU
// avoidance — the same reasoning ExpandHostCommandsForDispatch's doc
// comment gives for HostCommands), and WorkspaceStore.Load's contract
// should not implicitly depend on every caller getting a freshly allocated
// value each time (a future caching layer could reuse the same pointer
// across calls).
//
// This is tested directly at the helper level rather than through
// GetWithWorkspace end-to-end: both WorkspaceStore.Load backends (plain
// yaml and WorkspaceRepository) always allocate a brand new *WorkspaceMeta
// per call, so there is no black-box way to observe a pointer-aliasing bug
// through the public API today — the property only has teeth checked
// against the function whose contract it actually is.
func TestGetWithWorkspace_DoesNotMutateWorkspaceMetaInPlace(t *testing.T) {
	t.Setenv("PROBE_VAR", "expanded-value")

	original := &WorkspaceMeta{
		Env: map[string]string{"FOO": "${PROBE_VAR}"},
		AdditionalBindings: []BindMount{
			{Source: "${PROBE_VAR}/kit", Target: "${PROBE_VAR}/kit"},
		},
	}

	expanded := expandWorkspaceRuntimeForDispatch(original)

	if expanded.Env["FOO"] != "expanded-value" {
		t.Fatalf("expanded.Env[FOO] = %q, want expanded-value", expanded.Env["FOO"])
	}
	if original.Env["FOO"] != "${PROBE_VAR}" {
		t.Errorf("original.Env[FOO] mutated in place: got %q, want raw ${PROBE_VAR}", original.Env["FOO"])
	}

	if expanded.AdditionalBindings[0].Source != "expanded-value/kit" {
		t.Fatalf("expanded.AdditionalBindings[0].Source = %q, want expanded-value/kit", expanded.AdditionalBindings[0].Source)
	}
	if original.AdditionalBindings[0].Source != "${PROBE_VAR}/kit" {
		t.Errorf("original.AdditionalBindings[0].Source mutated in place: got %q, want raw ${PROBE_VAR}/kit", original.AdditionalBindings[0].Source)
	}
	if original.AdditionalBindings[0].Target != "${PROBE_VAR}/kit" {
		t.Errorf("original.AdditionalBindings[0].Target mutated in place: got %q, want raw ${PROBE_VAR}/kit", original.AdditionalBindings[0].Target)
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
