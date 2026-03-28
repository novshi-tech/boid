package hostcmd_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/hostcmd"
)

func TestCheckPolicy(t *testing.T) {
	tests := []struct {
		name     string
		def      hostcmd.CommandDef
		args     []string
		expected bool
	}{
		{
			name: "wildcard allows everything",
			def: hostcmd.CommandDef{
				AllowedPatterns: []string{"*"},
			},
			args:     []string{"--flag", "value"},
			expected: true,
		},
		{
			name: "specific pattern match",
			def: hostcmd.CommandDef{
				AllowedPatterns: []string{"--branch", "main"},
			},
			args:     []string{"--branch", "main"},
			expected: true,
		},
		{
			name: "pattern mismatch",
			def: hostcmd.CommandDef{
				AllowedPatterns: []string{"--safe-flag"},
			},
			args:     []string{"--dangerous-flag"},
			expected: false,
		},
		{
			name: "empty args always allowed",
			def: hostcmd.CommandDef{
				AllowedPatterns: []string{},
			},
			args:     []string{},
			expected: true,
		},
		{
			name: "glob pattern",
			def: hostcmd.CommandDef{
				AllowedPatterns: []string{"*.go"},
			},
			args:     []string{"main.go"},
			expected: true,
		},
		{
			name: "glob pattern no match",
			def: hostcmd.CommandDef{
				AllowedPatterns: []string{"*.go"},
			},
			args:     []string{"main.py"},
			expected: false,
		},
		{
			name: "empty patterns reject args",
			def: hostcmd.CommandDef{
				AllowedPatterns: []string{},
			},
			args:     []string{"something"},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := hostcmd.CheckPolicy(tt.def, tt.args)
			if result != tt.expected {
				t.Errorf("CheckPolicy(%v, %v) = %v, want %v", tt.def.AllowedPatterns, tt.args, result, tt.expected)
			}
		})
	}
}
