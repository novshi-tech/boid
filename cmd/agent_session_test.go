package cmd

import "testing"

// TestAgentSessionSubcommandsHaveProjectCompletion guards against the
// completion func being registered on the flag but not wired to a specific
// subcommand's help flag: RegisterFlagCompletionFunc failing silently (e.g.
// a duplicate name registered twice) would otherwise slip through
// unnoticed since `go build`/`go vet` don't catch a missing completion
// wiring.
func TestAgentSessionSubcommandsHaveProjectCompletion(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "opencode", "shell"} {
		t.Run(harness, func(t *testing.T) {
			sub, _, err := agentCmd.Find([]string{harness})
			if err != nil {
				t.Fatalf("find %q subcommand: %v", harness, err)
			}
			if sub.Name() != harness {
				t.Fatalf("expected subcommand %q, got %q", harness, sub.Name())
			}
			fn, ok := sub.GetFlagCompletionFunc("project")
			if !ok || fn == nil {
				t.Fatalf("boid agent %s -p: no completion func registered for --project", harness)
			}
		})
	}
}
