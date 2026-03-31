package sandbox_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestCheckPolicy(t *testing.T) {
	tests := []struct {
		name     string
		def      sandbox.CommandDef
		args     []string
		expected bool
	}{
		{
			name: "wildcard allows everything",
			def: sandbox.CommandDef{
				AllowedPatterns: []string{"*"},
			},
			args:     []string{"--flag", "value"},
			expected: true,
		},
		{
			name: "empty args always allowed",
			def: sandbox.CommandDef{
				AllowedPatterns: []string{},
			},
			args:     []string{},
			expected: true,
		},
		{
			name: "empty patterns reject args",
			def: sandbox.CommandDef{
				AllowedPatterns: []string{},
			},
			args:     []string{"something"},
			expected: false,
		},
		{
			name: "allowed pattern matches joined args",
			def: sandbox.CommandDef{
				AllowedPatterns: []string{"status *"},
			},
			args:     []string{"status", "--short"},
			expected: true,
		},
		{
			name: "denied pattern blocks",
			def: sandbox.CommandDef{
				AllowedPatterns: []string{"*"},
				DeniedPatterns:  []string{"push *://*"},
			},
			args:     []string{"push", "https://evil.com/repo"},
			expected: false,
		},
		{
			name: "denied pattern does not block non-matching",
			def: sandbox.CommandDef{
				AllowedPatterns: []string{"*"},
				DeniedPatterns:  []string{"push *://*"},
			},
			args:     []string{"push", "origin", "main"},
			expected: true,
		},
		{
			name: "allowed subcommands permit valid subcommand",
			def: sandbox.CommandDef{
				AllowedSubcommands: []string{"status", "log", "diff"},
				AllowedPatterns:    []string{"*"},
			},
			args:     []string{"status", "--short"},
			expected: true,
		},
		{
			name: "allowed subcommands block invalid subcommand",
			def: sandbox.CommandDef{
				AllowedSubcommands: []string{"status", "log"},
				AllowedPatterns:    []string{"*"},
			},
			args:     []string{"config", "--global", "user.email"},
			expected: false,
		},
		{
			name: "subcommand extraction skips global options",
			def: sandbox.CommandDef{
				AllowedSubcommands:  []string{"status"},
				AllowedPatterns:     []string{"*"},
				ExtractSubcommandFn: "git",
			},
			args:     []string{"-C", "/some/path", "status"},
			expected: true,
		},
		{
			name: "allowed subcommands without patterns permits valid subcommand",
			def: sandbox.CommandDef{
				AllowedSubcommands:  []string{"status", "log", "diff"},
				ExtractSubcommandFn: "git",
			},
			args:     []string{"status"},
			expected: true,
		},
		{
			name: "allowed subcommands without patterns blocks invalid subcommand",
			def: sandbox.CommandDef{
				AllowedSubcommands:  []string{"status", "log"},
				ExtractSubcommandFn: "git",
			},
			args:     []string{"config", "--global", "user.email"},
			expected: false,
		},
		{
			name: "allowed subcommands without patterns respects denied patterns",
			def: sandbox.CommandDef{
				AllowedSubcommands:  []string{"push"},
				DeniedPatterns:      []string{"push *://*"},
				ExtractSubcommandFn: "git",
			},
			args:     []string{"push", "https://evil.com/repo"},
			expected: false,
		},
		{
			name: "deny takes precedence over allow",
			def: sandbox.CommandDef{
				AllowedPatterns: []string{"*"},
				DeniedPatterns:  []string{"remote add *"},
			},
			args:     []string{"remote", "add", "evil"},
			expected: false,
		},
		{
			name: "default deny when no patterns match",
			def: sandbox.CommandDef{
				AllowedPatterns: []string{"status *"},
			},
			args:     []string{"push", "origin"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sandbox.CheckPolicy(tt.def, tt.args)
			if result != tt.expected {
				t.Errorf("CheckPolicy(%v, %v) = %v, want %v", tt.def, tt.args, result, tt.expected)
			}
		})
	}
}
