package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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
	if cfg.StateMachine.ReworkLimit != 5 {
		t.Errorf("default StateMachine.ReworkLimit: got %d, want 5", cfg.StateMachine.ReworkLimit)
	}
}

func TestLoadFromPath_StateMachineReworkLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
state_machine:
  rework_limit: 3
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StateMachine.ReworkLimit != 3 {
		t.Errorf("ReworkLimit: got %d, want 3", cfg.StateMachine.ReworkLimit)
	}
	// GC fields should retain defaults
	def := DefaultConfig()
	if cfg.GC.Enabled != def.GC.Enabled {
		t.Errorf("GC.Enabled: got %v, want default %v", cfg.GC.Enabled, def.GC.Enabled)
	}
}

func TestLoadFromPath_StateMachineReworkLimit_NotSet_UsesDefault(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
gc:
  interval: 12h
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadFromPath(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.StateMachine.ReworkLimit != 5 {
		t.Errorf("ReworkLimit: got %d, want default 5", cfg.StateMachine.ReworkLimit)
	}
}
