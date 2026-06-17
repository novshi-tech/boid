package cmd

import (
	"testing"
)

// TestParseTaskCreateSpec_ReadonlyTrue verifies that readonly: true in the YAML
// spec is no longer deprecated — it parses into CreateTaskRequest.Readonly.
func TestParseTaskCreateSpec_ReadonlyTrue(t *testing.T) {
	input := "project_id: p\ntitle: t\nbehavior: executor\nreadonly: true\n"
	req, err := parseTaskCreateSpec([]byte(input))
	if err != nil {
		t.Fatalf("parseTaskCreateSpec() error = %v", err)
	}
	if req.Readonly == nil {
		t.Fatal("Readonly = nil, want *true")
	}
	if !*req.Readonly {
		t.Errorf("*Readonly = false, want true")
	}
}

// TestParseTaskCreateSpec_ReadonlyFalse verifies that readonly: false also
// parses correctly (nil vs explicit false must be distinguishable).
func TestParseTaskCreateSpec_ReadonlyFalse(t *testing.T) {
	input := "project_id: p\ntitle: t\nbehavior: supervisor\nreadonly: false\n"
	req, err := parseTaskCreateSpec([]byte(input))
	if err != nil {
		t.Fatalf("parseTaskCreateSpec() error = %v", err)
	}
	if req.Readonly == nil {
		t.Fatal("Readonly = nil, want *false")
	}
	if *req.Readonly {
		t.Errorf("*Readonly = true, want false")
	}
}

// TestParseTaskCreateSpec_ReadonlyOmitted verifies that omitting readonly
// leaves CreateTaskRequest.Readonly as nil (so the server applies the behavior
// default and does not override it).
func TestParseTaskCreateSpec_ReadonlyOmitted(t *testing.T) {
	input := "project_id: p\ntitle: t\nbehavior: executor\n"
	req, err := parseTaskCreateSpec([]byte(input))
	if err != nil {
		t.Fatalf("parseTaskCreateSpec() error = %v", err)
	}
	if req.Readonly != nil {
		t.Errorf("Readonly = %v, want nil (omitted readonly must not set a value)", *req.Readonly)
	}
}
