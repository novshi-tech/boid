package dispatcher

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// Phase 5b PR1 (docs/plans/phase5-shim-and-task-context.md): WorkspaceEnvView
// is the reduced "environment" `boid task env` returns — only the two
// properties an in-sandbox agent cannot observe on its own
// (allowed_domains + host_commands) survive the 縮退 from the legacy
// environment.yaml. BuildWorkspaceEnvView must reuse the exact same
// convertHostCommands conversion buildEnvironmentYAML's host_commands
// section already uses, so the two representations cannot drift apart.

func TestBuildWorkspaceEnvView_AllowedDomainsCopied(t *testing.T) {
	in := []string{"github.com", "example.com"}
	view := BuildWorkspaceEnvView(in, nil)
	if len(view.AllowedDomains) != 2 || view.AllowedDomains[0] != "github.com" || view.AllowedDomains[1] != "example.com" {
		t.Fatalf("AllowedDomains = %v, want %v", view.AllowedDomains, in)
	}
	// Defensive copy: mutating the input slice must not affect the view.
	in[0] = "mutated.example"
	if view.AllowedDomains[0] != "github.com" {
		t.Errorf("AllowedDomains was not defensively copied: %v", view.AllowedDomains)
	}
}

func TestBuildWorkspaceEnvView_HostCommandsSortedDeterministic(t *testing.T) {
	hc := map[string]orchestrator.CommandDef{
		"gh":  {Name: "gh", AllowedSubcommands: []string{"pr", "issue"}},
		"aws": {Name: "aws"},
	}
	view := BuildWorkspaceEnvView(nil, hc)
	if len(view.HostCommands) != 2 {
		t.Fatalf("HostCommands = %+v, want 2 entries", view.HostCommands)
	}
	if view.HostCommands[0].Name != "aws" || view.HostCommands[1].Name != "gh" {
		t.Errorf("HostCommands order = [%s, %s], want [aws, gh]", view.HostCommands[0].Name, view.HostCommands[1].Name)
	}
	if len(view.HostCommands[1].Allow) != 2 {
		t.Errorf("gh.Allow = %v, want 2 entries (pr, issue)", view.HostCommands[1].Allow)
	}
}

func TestBuildWorkspaceEnvView_RejectRulesSurfaced(t *testing.T) {
	hc := map[string]orchestrator.CommandDef{
		"gh": {
			Name:               "gh",
			AllowedSubcommands: []string{"pr"},
			RejectRules: []orchestrator.RejectRule{
				{Match: "*--body-file*", Reason: `use --body "$(cat <file>)"`},
			},
		},
	}
	view := BuildWorkspaceEnvView(nil, hc)
	if len(view.HostCommands) != 1 {
		t.Fatalf("HostCommands = %+v, want 1 entry", view.HostCommands)
	}
	reject := view.HostCommands[0].Reject
	if len(reject) != 1 || reject[0].Match != "*--body-file*" || reject[0].Reason != `use --body "$(cat <file>)"` {
		t.Errorf("Reject = %+v, want the configured rule", reject)
	}
}

func TestBuildWorkspaceEnvView_EmptyInputsProduceEmptyView(t *testing.T) {
	view := BuildWorkspaceEnvView(nil, nil)
	if len(view.AllowedDomains) != 0 || len(view.HostCommands) != 0 {
		t.Errorf("view = %+v, want empty", view)
	}
}

// The JSON tags must be snake_case and match the `boid task env` RPC's wire
// contract (allowed_domains / host_commands / name / allow / deny / reject /
// match / reason) — this is a schema-stability guardrail (未解決論点 in the
// plan doc), not just a struct-shape check.
func TestWorkspaceEnvView_JSONSchema(t *testing.T) {
	view := WorkspaceEnvView{
		AllowedDomains: []string{"github.com"},
		HostCommands: []WorkspaceEnvHostCommand{
			{
				Name:  "gh",
				Allow: []string{"pr"},
				Deny:  []string{"api"},
				Reject: []WorkspaceEnvRejectRule{
					{Match: "*--body-file*", Reason: "no sandbox paths on host"},
				},
			},
		},
	}
	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["allowed_domains"]; !ok {
		t.Errorf("missing allowed_domains key: %s", raw)
	}
	hc, ok := m["host_commands"].([]any)
	if !ok || len(hc) != 1 {
		t.Fatalf("missing/malformed host_commands key: %s", raw)
	}
	entry := hc[0].(map[string]any)
	for _, key := range []string{"name", "allow", "deny", "reject"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("host_commands[0] missing key %q: %s", key, raw)
		}
	}
}
