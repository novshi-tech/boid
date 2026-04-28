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
			name: "allowed subcommands without patterns permits valid subcommand",
			def: sandbox.CommandDef{
				AllowedSubcommands: []string{"status", "log", "diff"},
			},
			args:     []string{"status"},
			expected: true,
		},
		{
			name: "allowed subcommands without patterns blocks invalid subcommand",
			def: sandbox.CommandDef{
				AllowedSubcommands: []string{"status", "log"},
			},
			args:     []string{"config", "--global", "user.email"},
			expected: false,
		},
		{
			name: "allowed subcommands without patterns respects denied patterns",
			def: sandbox.CommandDef{
				AllowedSubcommands: []string{"push"},
				DeniedPatterns:     []string{"push *://*"},
			},
			args:     []string{"push", "https://evil.com/repo"},
			expected: false,
		},
		{
			name: "allowed subcommands with flags-only args permitted",
			def: sandbox.CommandDef{
				AllowedSubcommands: []string{"status", "log"},
			},
			args:     []string{"--version"},
			expected: true,
		},
		{
			name: "allowed subcommands with flags-only args blocked by denied pattern",
			def: sandbox.CommandDef{
				AllowedSubcommands: []string{"status", "log"},
				DeniedPatterns:     []string{"--version"},
			},
			args:     []string{"--version"},
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

func TestCheckPolicy_GitKitPolicy(t *testing.T) {
	def := sandbox.CommandDef{
		AllowedSubcommands: []string{
			"status",
			"diff",
			"log",
			"add",
			"commit",
			"push",
			"pull",
			"fetch",
			"checkout",
			"branch",
			"merge",
			"rebase",
			"stash",
			"tag",
			"rev-parse",
			"show",
			"ls-files",
		},
		DeniedPatterns: []string{
			"-C *",
			"* -C *",
			"--git-dir *",
			"* --git-dir *",
			"--git-dir=*",
			"* --git-dir=*",
			"--work-tree *",
			"* --work-tree *",
			"--work-tree=*",
			"* --work-tree=*",
			"push *://*",
			"pull *://*",
			"fetch *://*",
			"push git@*:*",
			"pull git@*:*",
			"fetch git@*:*",
			"push file:*",
			"pull file:*",
			"fetch file:*",
			"push /*",
			"pull /*",
			"fetch /*",
			"push ./*",
			"pull ./*",
			"fetch ./*",
			"push ../*",
			"pull ../*",
			"fetch ../*",
			"push ~/*",
			"pull ~/*",
			"fetch ~/*",
		},
	}

	tests := []struct {
		name     string
		args     []string
		expected bool
	}{
		{
			name:     "allow flags-only args like --version",
			args:     []string{"--version"},
			expected: true,
		},
		{
			name:     "allow push to configured remote",
			args:     []string{"push", "origin", "main"},
			expected: true,
		},
		{
			name:     "allow pull with options",
			args:     []string{"pull", "--ff-only", "origin", "main"},
			expected: true,
		},
		{
			name:     "allow fetch from configured remote",
			args:     []string{"fetch", "origin"},
			expected: true,
		},
		{
			name:     "deny remote subcommand",
			args:     []string{"remote", "-v"},
			expected: false,
		},
		{
			name:     "deny clone subcommand",
			args:     []string{"clone", "https://github.com/acme/repo.git"},
			expected: false,
		},
		{
			name:     "deny explicit repo switch via -C",
			args:     []string{"-C", "/tmp/other-repo", "status"},
			expected: false,
		},
		{
			name:     "deny explicit repo switch via --git-dir",
			args:     []string{"--git-dir=/tmp/other-repo/.git", "status"},
			expected: false,
		},
		{
			name:     "deny explicit repo switch via --work-tree",
			args:     []string{"--work-tree", "/tmp/other-repo", "status"},
			expected: false,
		},
		{
			name:     "deny push to direct url",
			args:     []string{"push", "https://github.com/acme/other.git", "main"},
			expected: false,
		},
		{
			name:     "deny fetch from ssh target",
			args:     []string{"fetch", "git@github.com:acme/other.git"},
			expected: false,
		},
		{
			name:     "deny pull from relative path repo",
			args:     []string{"pull", "../other-repo"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sandbox.CheckPolicy(def, tt.args)
			if result != tt.expected {
				t.Errorf("CheckPolicy(%v, %v) = %v, want %v", def, tt.args, result, tt.expected)
			}
		})
	}
}
