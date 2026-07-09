package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/gitgateway"
)

func TestLoadFromPath_FileNotExist_ReturnsDefaults(t *testing.T) {
	cfg, err := loadFromPath(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	def := DefaultConfig()
	if cfg.GC.Enabled != def.GC.Enabled {
		t.Errorf("Enabled: got %v, want %v", cfg.GC.Enabled, def.GC.Enabled)
	}
	if cfg.GC.Interval != def.GC.Interval {
		t.Errorf("Interval: got %v, want %v", cfg.GC.Interval, def.GC.Interval)
	}
	if cfg.GC.OlderThan != def.GC.OlderThan {
		t.Errorf("OlderThan: got %v, want %v", cfg.GC.OlderThan, def.GC.OlderThan)
	}
}

func TestLoadFromPath_ValidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gc:
  enabled: false
  interval: 12h
  older_than: 360h
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.GC.Enabled != false {
		t.Errorf("Enabled: got %v, want false", cfg.GC.Enabled)
	}
	if cfg.GC.Interval != 12*time.Hour {
		t.Errorf("Interval: got %v, want 12h", cfg.GC.Interval)
	}
	if cfg.GC.OlderThan != 360*time.Hour {
		t.Errorf("OlderThan: got %v, want 360h", cfg.GC.OlderThan)
	}
}

func TestLoadFromPath_TaskAskDisconnectGrace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
task_ask:
  disconnect_grace: 45m
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TaskAsk.DisconnectGrace != 45*time.Minute {
		t.Errorf("DisconnectGrace: got %v, want 45m", cfg.TaskAsk.DisconnectGrace)
	}
}

// An unset task_ask falls back to the 30m default.
func TestLoadFromPath_TaskAskDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("gc:\n  interval: 6h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TaskAsk.DisconnectGrace != 30*time.Minute {
		t.Errorf("DisconnectGrace default: got %v, want 30m", cfg.TaskAsk.DisconnectGrace)
	}
}

func TestLoadFromPath_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("gc: [invalid"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadFromPath(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestLoadFromPath_PartialYAML_UsesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// only set interval, others should be defaults
	content := `
gc:
  interval: 6h
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	def := DefaultConfig()
	if cfg.GC.Enabled != def.GC.Enabled {
		t.Errorf("Enabled: got %v, want %v (default)", cfg.GC.Enabled, def.GC.Enabled)
	}
	if cfg.GC.Interval != 6*time.Hour {
		t.Errorf("Interval: got %v, want 6h", cfg.GC.Interval)
	}
	if cfg.GC.OlderThan != def.GC.OlderThan {
		t.Errorf("OlderThan: got %v, want %v (default)", cfg.GC.OlderThan, def.GC.OlderThan)
	}
}

func TestLoadFromPath_InvalidDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gc:
  interval: notaduration
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := loadFromPath(path)
	if err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
}

func TestLoadFromPath_GatewayHosts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  hosts:
    - host: github.com
      forge: github
      secret_key: gh-pat
    - host: bitbucket.org
      forge: bitbucket
      secret_key: bb-token
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Gateway.Hosts) != 2 {
		t.Fatalf("Gateway.Hosts = %#v, want 2 entries", cfg.Gateway.Hosts)
	}
	gh := cfg.Gateway.Hosts[0]
	if gh.Host != "github.com" || gh.Forge != gitgateway.ForgeGitHub || gh.SecretKey != "gh-pat" {
		t.Errorf("Gateway.Hosts[0] = %#v, want {github.com github gh-pat}", gh)
	}
	bb := cfg.Gateway.Hosts[1]
	if bb.Host != "bitbucket.org" || bb.Forge != gitgateway.ForgeBitbucket || bb.SecretKey != "bb-token" {
		t.Errorf("Gateway.Hosts[1] = %#v, want {bitbucket.org bitbucket bb-token}", bb)
	}
	// config.yaml never carries a plaintext token; Scheme is a test-only
	// override (yaml:"-") and must stay unset from a real config file.
	if gh.Scheme != "" {
		t.Errorf("Gateway.Hosts[0].Scheme = %q, want empty (not yaml-settable)", gh.Scheme)
	}
}

func TestLoadFromPath_GatewayHosts_UnrecognizedForgeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  hosts:
    - host: gitlab.com
      forge: gitlab
      secret_key: gl-pat
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFromPath(path); err == nil {
		t.Fatal("expected error for unrecognized forge, got nil")
	}
}

func TestLoadFromPath_GatewayHosts_MissingSecretKeyRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gateway:
  hosts:
    - host: github.com
      forge: github
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFromPath(path); err == nil {
		t.Fatal("expected error for missing secret_key, got nil")
	}
}

// An unset gateway block must not error and must leave Hosts empty — the
// gateway is still constructed (PR4's lifecycle wiring is unconditional) but
// with no forge credentials configured.
func TestLoadFromPath_GatewayHosts_UnsetIsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("gc:\n  interval: 6h\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Gateway.Hosts) != 0 {
		t.Errorf("Gateway.Hosts = %#v, want empty", cfg.Gateway.Hosts)
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.GC.Enabled {
		t.Error("default GC.Enabled should be true")
	}
	if cfg.GC.Interval != 24*time.Hour {
		t.Errorf("default GC.Interval: got %v, want 24h", cfg.GC.Interval)
	}
	if cfg.GC.OlderThan != 720*time.Hour {
		t.Errorf("default GC.OlderThan: got %v, want 720h", cfg.GC.OlderThan)
	}
}
