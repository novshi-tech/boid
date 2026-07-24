package config

import (
	"strings"
	"testing"
)

func TestValidateYAML_Valid(t *testing.T) {
	data := []byte(`
sandbox:
  allowed_domains:
    - .freee.co.jp
    - api.example.com
gateway:
  forges:
    github:
      secret_key: gh-pat
`)
	cfg, err := ValidateYAML(data)
	if err != nil {
		t.Fatalf("ValidateYAML: %v", err)
	}
	if len(cfg.Sandbox.AllowedDomains) != 2 {
		t.Errorf("AllowedDomains = %v", cfg.Sandbox.AllowedDomains)
	}
}

func TestValidateYAML_EmptyDocument(t *testing.T) {
	cfg, err := ValidateYAML(nil)
	if err != nil {
		t.Fatalf("ValidateYAML(nil): %v", err)
	}
	if cfg.Sandbox.Backend != SandboxBackendUserns {
		t.Errorf("expected default backend, got %v", cfg.Sandbox.Backend)
	}
}

func TestValidateYAML_UnknownTopLevelKey(t *testing.T) {
	data := []byte("default_harness: claude-code\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for unknown top-level key default_harness")
	}
	if !strings.Contains(err.Error(), "default_harness") {
		t.Errorf("expected error to name default_harness, got: %v", err)
	}
}

func TestValidateYAML_UnknownNestedKey(t *testing.T) {
	data := []byte("sandbox:\n  alowed_domains:\n    - x.com\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for unknown nested key")
	}
	if !strings.Contains(err.Error(), "sandbox.alowed_domains") {
		t.Errorf("expected error to name sandbox.alowed_domains, got: %v", err)
	}
	if !strings.Contains(err.Error(), "did you mean") {
		t.Errorf("expected suggestion, got: %v", err)
	}
}

func TestValidateYAML_UnknownForgeField(t *testing.T) {
	data := []byte("gateway:\n  forges:\n    github:\n      hots: github.com\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for unknown forge field")
	}
	if !strings.Contains(err.Error(), "gateway.forges.github.hots") {
		t.Errorf("expected error to name the full path, got: %v", err)
	}
}

func TestValidateYAML_InvalidSandboxBackend(t *testing.T) {
	data := []byte("sandbox:\n  backend: bogus\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for invalid sandbox.backend")
	}
}

func TestValidateYAML_IncompleteCustomForge(t *testing.T) {
	// A custom forge id with no host is rejected by the existing
	// resolveForgeConfig invariant (config.go), exercised here through
	// ValidateYAML's structural decode pass.
	data := []byte("gateway:\n  forges:\n    my-forge:\n      secret_key: x\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for incomplete custom forge entry")
	}
}

func TestValidateYAML_InvalidDomainSyntax(t *testing.T) {
	cases := []string{
		"http://example.com",
		"example.com/path",
		"exa mple.com",
		"example.com:8080",
		".",
		"",
	}
	for _, d := range cases {
		data := []byte("sandbox:\n  allowed_domains:\n    - \"" + d + "\"\n")
		if _, err := ValidateYAML(data); err == nil {
			t.Errorf("expected error for invalid domain entry %q", d)
		}
	}
}

func TestValidateDomainEntry_Valid(t *testing.T) {
	valid := []string{".freee.co.jp", "api.example.com", ".docker.io", "localhost"}
	for _, d := range valid {
		if err := ValidateDomainEntry(d); err != nil {
			t.Errorf("ValidateDomainEntry(%q) = %v, want nil", d, err)
		}
	}
}

func TestValidateYAML_MapShapeError(t *testing.T) {
	data := []byte("sandbox: not-a-mapping\n")
	_, err := ValidateYAML(data)
	if err == nil {
		t.Fatal("expected error for sandbox: <scalar>")
	}
}
