package hostcmd_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/hostcmd"
)

func TestCommandFromArgv0(t *testing.T) {
	tests := []struct {
		argv0    string
		expected string
	}{
		{"/usr/bin/git", "git"},
		{"/usr/local/bin/boid", "boid"},
		{"./relative/path/to/cmd", "cmd"},
		{"simple", "simple"},
		{"/opt/boid/bin/gh", "gh"},
	}

	for _, tt := range tests {
		t.Run(tt.argv0, func(t *testing.T) {
			got := hostcmd.CommandFromArgv0(tt.argv0)
			if got != tt.expected {
				t.Errorf("CommandFromArgv0(%q) = %q, want %q", tt.argv0, got, tt.expected)
			}
		})
	}
}
