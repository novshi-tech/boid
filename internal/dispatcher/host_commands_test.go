package dispatcher

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// TestResolveHostCommands_RejectRulesPassthrough guards the resolve seam:
// ResolveHostCommands rewrites Name/Path on a struct copy, so every other
// field (including reject rules) must survive untouched. If the copy is ever
// replaced with field-by-field construction, this test catches dropped fields.
func TestResolveHostCommands_RejectRulesPassthrough(t *testing.T) {
	in := map[string]orchestrator.CommandDef{
		"gh": {
			AllowedSubcommands: []string{"pr"},
			RejectRules: []orchestrator.RejectRule{
				{Match: "*--body-file*", Reason: "sandbox paths are not visible on the host"},
			},
		},
	}
	lookPath := func(name string) (string, error) { return "/usr/bin/" + name, nil }

	out, err := ResolveHostCommands(nil, in, "/proj", lookPath)
	if err != nil {
		t.Fatalf("ResolveHostCommands: %v", err)
	}
	def, ok := out["/usr/bin/gh"]
	if !ok {
		t.Fatalf("missing resolved gh entry: %+v", out)
	}
	if def.Name != "gh" || def.Path != "/usr/bin/gh" {
		t.Fatalf("unexpected resolved identity: %+v", def)
	}
	if len(def.RejectRules) != 1 || def.RejectRules[0] != in["gh"].RejectRules[0] {
		t.Fatalf("reject rules dropped across resolve: %+v", def.RejectRules)
	}
}
