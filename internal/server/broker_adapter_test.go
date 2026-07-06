package server

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestToSandboxCommandDefs_FieldPassthrough guards the orchestrator→sandbox
// CommandDef conversion seam. The two structs are deliberately decoupled
// mirrors (sandbox does not import orchestrator), so every field added to the
// transport shape must be threaded through here by hand — this test fails
// when a field is added upstream but dropped in the conversion.
func TestToSandboxCommandDefs_FieldPassthrough(t *testing.T) {
	in := map[string]orchestrator.CommandDef{
		"gh": {
			Name:               "gh",
			Path:               "/usr/bin/gh",
			AllowedPatterns:    []string{"pr create *"},
			DeniedPatterns:     []string{"repo delete *"},
			AllowedSubcommands: []string{"pr", "issue"},
			Env:                map[string]string{"GH_TOKEN": "tok"},
			RejectRules: []orchestrator.RejectRule{
				{Match: "*--body-file*", Reason: "sandbox paths are not visible on the host"},
				{Match: "* --web*", Reason: "no browser in the sandbox"},
			},
		},
	}

	out := toSandboxCommandDefs(in)
	got, ok := out["gh"]
	if !ok {
		t.Fatalf("missing gh entry: %+v", out)
	}
	if got.Name != "gh" || got.Path != "/usr/bin/gh" {
		t.Fatalf("scalar fields dropped in conversion: %+v", got)
	}
	if len(got.AllowedPatterns) != 1 || len(got.DeniedPatterns) != 1 || len(got.AllowedSubcommands) != 2 {
		t.Fatalf("pattern fields dropped in conversion: %+v", got)
	}
	if got.Env["GH_TOKEN"] != "tok" {
		t.Fatalf("env dropped in conversion: %+v", got.Env)
	}
	if len(got.RejectRules) != 2 {
		t.Fatalf("reject rules dropped in conversion: %+v", got.RejectRules)
	}
	for i, want := range in["gh"].RejectRules {
		if got.RejectRules[i].Match != want.Match || got.RejectRules[i].Reason != want.Reason {
			t.Fatalf("reject rule %d mismatch: got %+v, want %+v", i, got.RejectRules[i], want)
		}
	}
}

// TestToSandboxCommandDefs_Empty verifies the nil-map fast path is preserved.
func TestToSandboxCommandDefs_Empty(t *testing.T) {
	if out := toSandboxCommandDefs(nil); out != nil {
		t.Fatalf("expected nil for empty input, got %+v", out)
	}
}
