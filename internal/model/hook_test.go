package model_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/model"
	"gopkg.in/yaml.v3"
)

func TestGate_YAMLRoundTrip(t *testing.T) {
	data := `
id: push-pr
on: executing
requires_traits:
  - artifact
`
	var gate model.Gate
	if err := yaml.Unmarshal([]byte(data), &gate); err != nil {
		t.Fatalf("unmarshal yaml: %v", err)
	}

	if gate.ID != "push-pr" {
		t.Fatalf("expected id push-pr, got %s", gate.ID)
	}
	if gate.On != "executing" {
		t.Fatalf("expected on executing, got %s", gate.On)
	}
	if len(gate.RequiresTraits) != 1 || gate.RequiresTraits[0] != model.TraitArtifact {
		t.Fatalf("unexpected requires_traits: %v", gate.RequiresTraits)
	}
}

func TestGate_JSONRoundTrip(t *testing.T) {
	original := model.Gate{
		ID:             "ci-check",
		On:             "verifying",
		RequiresTraits: []model.TraitType{model.TraitArtifact},
	}

	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded model.Gate
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if decoded.ID != original.ID {
		t.Fatalf("expected id %s, got %s", original.ID, decoded.ID)
	}
	if decoded.On != original.On {
		t.Fatalf("expected on %s, got %s", original.On, decoded.On)
	}
	if len(decoded.RequiresTraits) != 1 || decoded.RequiresTraits[0] != model.TraitArtifact {
		t.Fatalf("unexpected requires_traits: %v", decoded.RequiresTraits)
	}
}

func TestValidGateOnValues(t *testing.T) {
	for _, v := range []string{
		"pending", "executing", "verifying",
		"in_review", "collecting_feedback", "done", "aborted",
	} {
		if !model.ValidGateOnValues[v] {
			t.Errorf("expected %q to be valid gate on value", v)
		}
	}

	if model.ValidGateOnValues["invalid"] {
		t.Error("expected 'invalid' to be invalid gate on value")
	}
}

func TestResolveGateScript(t *testing.T) {
	dir := t.TempDir()
	gatesDir := filepath.Join(dir, "gates")
	os.MkdirAll(gatesDir, 0o755)

	// Create a .sh gate script
	shPath := filepath.Join(gatesDir, "push-pr.sh")
	os.WriteFile(shPath, []byte("#!/bin/bash\n"), 0o755)

	got, err := model.ResolveGateScript(gatesDir, "push-pr")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != shPath {
		t.Fatalf("expected %s, got %s", shPath, got)
	}

	// Create a .py gate script
	pyPath := filepath.Join(gatesDir, "ci-check.py")
	os.WriteFile(pyPath, []byte("#!/usr/bin/env python3\n"), 0o755)

	got, err = model.ResolveGateScript(gatesDir, "ci-check")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != pyPath {
		t.Fatalf("expected %s, got %s", pyPath, got)
	}

	// Missing script
	_, err = model.ResolveGateScript(gatesDir, "nonexistent")
	if err == nil {
		t.Fatal("expected error for missing gate script")
	}
}

func TestRole_Constants(t *testing.T) {
	if model.RoleHook != "hook" {
		t.Fatalf("expected RoleHook=hook, got %s", model.RoleHook)
	}
	if model.RoleGate != "gate" {
		t.Fatalf("expected RoleGate=gate, got %s", model.RoleGate)
	}
}
