package cmd

import "testing"

// TestAgentSessionOutputFlag_ShorthandMatchesRootPersistentFlag guards
// against the local --output flag's shorthand silently dropping out from a
// pflag name collision with the root persistent -o/--output flag
// (cmd/root.go): registering the local flag without its own "o" shorthand
// leaves -o bound to neither flag, so `boid agent claude -o json` fails
// with "unknown shorthand flag" even though --output alone still works.
// ParseFlags (unlike a bare Flags() call) exercises the real
// mergePersistentFlags() path Execute() uses, so this reproduces the same
// failure a real invocation would hit.
func TestAgentSessionOutputFlag_ShorthandMatchesRootPersistentFlag(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "opencode"} {
		t.Run(harness, func(t *testing.T) {
			sub, _, err := agentCmd.Find([]string{harness})
			if err != nil {
				t.Fatalf("find %q subcommand: %v", harness, err)
			}
			if err := sub.ParseFlags([]string{"-o", "json", "--no-attach", "-p", "proj"}); err != nil {
				t.Fatalf("boid agent %s -o json --no-attach -p proj: %v", harness, err)
			}
			if got, _ := sub.Flags().GetString("output"); got != "json" {
				t.Errorf("--output = %q, want %q (via -o shorthand)", got, "json")
			}
		})
	}
}

// TestAgentSessionSubcommandsHaveProjectCompletion guards against the
// completion func being registered on the flag but not wired to a specific
// subcommand's help flag: RegisterFlagCompletionFunc failing silently (e.g.
// a duplicate name registered twice) would otherwise slip through
// unnoticed since `go build`/`go vet` don't catch a missing completion
// wiring.
func TestAgentSessionSubcommandsHaveProjectCompletion(t *testing.T) {
	for _, harness := range []string{"claude", "codex", "opencode"} {
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
