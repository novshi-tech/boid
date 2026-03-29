package hostcmd

import "testing"

func TestExtractGitSubcommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"simple subcommand", []string{"status"}, "status"},
		{"with flags after", []string{"log", "--oneline"}, "log"},
		{"-C option", []string{"-C", "/path", "diff"}, "diff"},
		{"-c option", []string{"-c", "core.pager=less", "log"}, "log"},
		{"--git-dir", []string{"--git-dir", "/path/.git", "status"}, "status"},
		{"--git-dir=value", []string{"--git-dir=/path/.git", "status"}, "status"},
		{"--work-tree", []string{"--work-tree", "/path", "status"}, "status"},
		{"multiple global opts", []string{"-C", "/path", "-c", "k=v", "push"}, "push"},
		{"bare flag", []string{"--no-pager", "log"}, "log"},
		{"no subcommand", []string{"-C", "/path"}, ""},
		{"empty args", []string{}, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractGitSubcommand(tt.args)
			if got != tt.want {
				t.Errorf("extractGitSubcommand(%v) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "anything", true},
		{"*", "with/slash", true},
		{"*", "", true},
		{"push *://*", "push https://evil.com/repo", true},
		{"push *://*", "push origin main", false},
		{"push *@*:*", "push git@github.com:user/repo", true},
		{"status *", "status --short", true},
		{"status *", "log --oneline", false},
		{"remote add *", "remote add evil", true},
		{"remote add *", "remote remove evil", false},
		{"", "", true},
		{"", "x", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+"_"+tt.s, func(t *testing.T) {
			got := globMatch(tt.pattern, tt.s)
			if got != tt.want {
				t.Errorf("globMatch(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
			}
		})
	}
}
