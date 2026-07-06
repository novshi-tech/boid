package sandbox_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestEarlyReject covers the shim-side fast path: a matching rule produces
// the exact same "host_commands.<name>: rejected: <reason>" message the
// broker would, while any malformed/absent/non-matching input falls through
// (never blocks) so the broker remains the sole authority in those cases.
func TestEarlyReject(t *testing.T) {
	rulesJSON := `{"gh":[{"match":"*--body-file*","reason":"sandbox paths are not visible on the host"},{"match":"*--web*","reason":"no browser in the sandbox"}]}`

	tests := []struct {
		name       string
		raw        string
		command    string
		args       []string
		wantMsg    string
		wantReject bool
	}{
		{
			name:       "matching rule rejects with broker-identical message",
			raw:        rulesJSON,
			command:    "gh",
			args:       []string{"pr", "create", "--body-file", "/tmp/x"},
			wantMsg:    "host_commands.gh: rejected: sandbox paths are not visible on the host",
			wantReject: true,
		},
		{
			name:       "second rule in list also matches",
			raw:        rulesJSON,
			command:    "gh",
			args:       []string{"pr", "create", "--web"},
			wantMsg:    "host_commands.gh: rejected: no browser in the sandbox",
			wantReject: true,
		},
		{
			name:       "no matching rule passes through",
			raw:        rulesJSON,
			command:    "gh",
			args:       []string{"pr", "list"},
			wantReject: false,
		},
		{
			name:       "unknown command passes through",
			raw:        rulesJSON,
			command:    "git",
			args:       []string{"push", "--force"},
			wantReject: false,
		},
		{
			name:       "empty env passes through",
			raw:        "",
			command:    "gh",
			args:       []string{"pr", "create", "--web"},
			wantReject: false,
		},
		{
			name:       "malformed JSON passes through",
			raw:        `{"gh":[{"match":`,
			command:    "gh",
			args:       []string{"pr", "create", "--web"},
			wantReject: false,
		},
		{
			name:       "empty command passes through",
			raw:        rulesJSON,
			command:    "",
			args:       []string{"pr", "create", "--web"},
			wantReject: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMsg, gotReject := sandbox.EarlyReject(tt.raw, tt.command, tt.args)
			if gotReject != tt.wantReject {
				t.Fatalf("EarlyReject() rejected = %v, want %v (msg=%q)", gotReject, tt.wantReject, gotMsg)
			}
			if tt.wantReject && gotMsg != tt.wantMsg {
				t.Errorf("EarlyReject() msg = %q, want %q", gotMsg, tt.wantMsg)
			}
			if !tt.wantReject && gotMsg != "" {
				t.Errorf("EarlyReject() msg = %q, want empty on pass-through", gotMsg)
			}
		})
	}
}

// TestEarlyRejectFromEnv verifies the env-reading wrapper picks up
// BOID_HOST_COMMAND_RULES exactly as EarlyReject would from the raw string.
func TestEarlyRejectFromEnv(t *testing.T) {
	t.Setenv(sandbox.HostCommandRulesEnv, `{"gh":[{"match":"*--web*","reason":"no browser in the sandbox"}]}`)

	msg, rejected := sandbox.EarlyRejectFromEnv("gh", []string{"pr", "create", "--web"})
	if !rejected {
		t.Fatalf("expected rejection")
	}
	want := "host_commands.gh: rejected: no browser in the sandbox"
	if msg != want {
		t.Errorf("msg = %q, want %q", msg, want)
	}

	msg, rejected = sandbox.EarlyRejectFromEnv("gh", []string{"pr", "list"})
	if rejected {
		t.Errorf("expected pass-through, got rejected with msg %q", msg)
	}
}

// TestEarlyRejectFromEnv_Unset verifies EarlyRejectFromEnv never blocks when
// BOID_HOST_COMMAND_RULES isn't set at all (the common case: most sandboxes
// declare no reject rules).
func TestEarlyRejectFromEnv_Unset(t *testing.T) {
	t.Setenv(sandbox.HostCommandRulesEnv, "")

	msg, rejected := sandbox.EarlyRejectFromEnv("gh", []string{"pr", "create", "--web"})
	if rejected {
		t.Errorf("expected pass-through when env unset, got rejected with msg %q", msg)
	}
}
