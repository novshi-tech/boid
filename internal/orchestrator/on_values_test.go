package orchestrator_test

import (
	"encoding/json"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

func TestOnValues_UnmarshalYAML_Scalar(t *testing.T) {
	type S struct {
		On projectspec.OnValues `yaml:"on"`
	}
	var s S
	if err := yaml.Unmarshal([]byte("on: executing"), &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.On) != 1 || s.On[0] != "executing" {
		t.Fatalf("expected [executing], got %v", s.On)
	}
}

func TestOnValues_UnmarshalYAML_Sequence(t *testing.T) {
	type S struct {
		On projectspec.OnValues `yaml:"on"`
	}
	var s S
	if err := yaml.Unmarshal([]byte("on: [executing, reworking]"), &s); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(s.On) != 2 || s.On[0] != "executing" || s.On[1] != "reworking" {
		t.Fatalf("expected [executing, reworking], got %v", s.On)
	}
}

func TestOnValues_UnmarshalYAML_InvalidType(t *testing.T) {
	type S struct {
		On projectspec.OnValues `yaml:"on"`
	}
	var s S
	if err := yaml.Unmarshal([]byte("on:\n  key: val"), &s); err == nil {
		t.Fatal("expected error for mapping type, got nil")
	}
}

func TestOnValues_Contains(t *testing.T) {
	o := projectspec.OnValues{"executing", "reworking"}
	if !o.Contains("executing") {
		t.Error("expected Contains(executing) == true")
	}
	if !o.Contains("reworking") {
		t.Error("expected Contains(reworking) == true")
	}
	if o.Contains("done") {
		t.Error("expected Contains(done) == false")
	}
}

func TestOnValues_AllValid(t *testing.T) {
	valid := map[string]bool{"executing": true, "reworking": true}
	o := projectspec.OnValues{"executing", "reworking"}
	if !o.AllValid(valid) {
		t.Error("expected AllValid == true")
	}
	bad := projectspec.OnValues{"executing", "invalid_status"}
	if bad.AllValid(valid) {
		t.Error("expected AllValid == false for invalid_status")
	}
}

func TestOnValues_MarshalJSON(t *testing.T) {
	o := projectspec.OnValues{"executing", "reworking"}
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(b) != `["executing","reworking"]` {
		t.Fatalf("unexpected JSON: %s", b)
	}
}
