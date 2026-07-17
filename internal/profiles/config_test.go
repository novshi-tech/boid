package profiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfigFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	return path
}

func TestLoadConfig_MissingFile_ReturnsEmptyConfig(t *testing.T) {
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultProfile != "" || len(cfg.Profiles) != 0 {
		t.Errorf("expected empty Config, got %+v", cfg)
	}
}

func TestLoadConfig_EmptyFile_ReturnsEmptyConfig(t *testing.T) {
	path := writeConfigFile(t, "")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultProfile != "" || len(cfg.Profiles) != 0 {
		t.Errorf("expected empty Config, got %+v", cfg)
	}
}

func TestLoadConfig_ParsesProfilesAndDefault(t *testing.T) {
	path := writeConfigFile(t, `
default_profile: home
profiles:
  home:
    url: unix:///run/user/1000/boid.sock
  work:
    url: https://work.example.com
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultProfile != "home" {
		t.Errorf("DefaultProfile = %q, want %q", cfg.DefaultProfile, "home")
	}
	if len(cfg.Profiles) != 2 {
		t.Fatalf("expected 2 profiles, got %d: %+v", len(cfg.Profiles), cfg.Profiles)
	}
	if got := cfg.Profiles["home"].URL; got != "unix:///run/user/1000/boid.sock" {
		t.Errorf("home url = %q", got)
	}
	if got := cfg.Profiles["work"].URL; got != "https://work.example.com" {
		t.Errorf("work url = %q", got)
	}
}

// TestLoadConfig_TolerantOfUnrelatedTopLevelSections pins the core design
// constraint: config.yaml is shared by internal/config.Config's own
// gc/web/notify/sandbox/task_ask/gateway sections (boid web set-url writes
// into the very same file). A naive top-level KnownFields(true) decode
// targeting only {default_profile, profiles} would reject every config.yaml
// that also has, say, a gateway.forges block — which real dogfood configs
// already do. Config.UnmarshalYAML must silently ignore those siblings.
func TestLoadConfig_TolerantOfUnrelatedTopLevelSections(t *testing.T) {
	path := writeConfigFile(t, `
web:
  public_url: https://boid.example.com
  http_addr: 127.0.0.1:8080
gc:
  enabled: true
gateway:
  forges:
    github: {}
default_profile: home
profiles:
  home:
    url: unix:///run/user/1000/boid.sock
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig should tolerate unrelated top-level sections, got error: %v", err)
	}
	if cfg.DefaultProfile != "home" {
		t.Errorf("DefaultProfile = %q, want %q", cfg.DefaultProfile, "home")
	}
	if got := cfg.Profiles["home"].URL; got != "unix:///run/user/1000/boid.sock" {
		t.Errorf("home url = %q", got)
	}
}

// TestLoadConfig_RejectsUnknownFieldWithinProfile pins the "strict WITHIN a
// profile entry" half of the same design: a typo'd field inside a single
// profiles.<name> mapping must be rejected, even though sibling top-level
// sections (the test above) are tolerated.
func TestLoadConfig_RejectsUnknownFieldWithinProfile(t *testing.T) {
	path := writeConfigFile(t, `
profiles:
  work:
    urll: https://work.example.com
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected an error for an unknown field within a profile entry")
	}
	if !strings.Contains(err.Error(), "work") {
		t.Errorf("error should name the offending profile, got %q", err.Error())
	}
}

func TestLoadConfig_RejectsUnknownFieldWithinProfile_MultipleProfiles(t *testing.T) {
	path := writeConfigFile(t, `
profiles:
  home:
    url: unix:///run/user/1000/boid.sock
  work:
    url: https://work.example.com
    token: should-not-be-here
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected an error: token is not a recognized field on a profile entry")
	}
}
