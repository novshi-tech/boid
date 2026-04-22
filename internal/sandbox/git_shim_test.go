package sandbox

import "testing"

func TestClassifyGitInvocation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantMode   gitInvocationMode
		wantOp     GitOp
		wantRemote string
		wantErr    bool
	}{
		{
			name:     "local status",
			args:     []string{"status", "--short"},
			wantMode: gitInvocationLocal,
		},
		{
			name:     "local no args",
			args:     nil,
			wantMode: gitInvocationLocal,
		},
		{
			name:       "brokered fetch",
			args:       []string{"fetch", "--prune", "origin"},
			wantMode:   gitInvocationBrokered,
			wantOp:     GitOpFetch,
			wantRemote: "origin",
		},
		{
			name:       "brokered push",
			args:       []string{"push", "--force-with-lease", "origin", "main"},
			wantMode:   gitInvocationBrokered,
			wantOp:     GitOpPush,
			wantRemote: "origin",
		},
		{
			name:     "local worktree list",
			args:     []string{"worktree", "list"},
			wantMode: gitInvocationLocal,
		},
		{
			name:     "local merge-tree",
			args:     []string{"merge-tree", "--write-tree", "HEAD", "MERGE_HEAD"},
			wantMode: gitInvocationLocal,
		},
		{
			name:     "local update-ref",
			args:     []string{"update-ref", "refs/heads/main", "abc123"},
			wantMode: gitInvocationLocal,
		},
		{
			name:    "deny pull",
			args:    []string{"pull", "origin", "main"},
			wantErr: true,
		},
		{
			name:    "deny global repo override",
			args:    []string{"-C", "/tmp/other", "status"},
			wantErr: true,
		},
		{
			name:    "deny force push option",
			args:    []string{"push", "--force", "origin", "main"},
			wantErr: true,
		},
		{
			name:    "deny fetch option after remote",
			args:    []string{"fetch", "origin", "--prune"},
			wantErr: true,
		},
		// git config cases
		{
			name:     "config --get remote url allowed",
			args:     []string{"config", "--get", "remote.origin.url"},
			wantMode: gitInvocationLocal,
		},
		{
			name:     "config --list allowed",
			args:     []string{"config", "--list"},
			wantMode: gitInvocationLocal,
		},
		{
			name:     "config user.name write allowed",
			args:     []string{"config", "user.name", "Foo"},
			wantMode: gitInvocationLocal,
		},
		{
			name:    "config remote.origin.url write denied",
			args:    []string{"config", "remote.origin.url", "https://evil"},
			wantErr: true,
		},
		{
			name:    "config core.hooksPath write denied",
			args:    []string{"config", "core.hooksPath", "/tmp"},
			wantErr: true,
		},
		{
			name:    "config core.sshCommand write denied",
			args:    []string{"config", "core.sshCommand", "evil"},
			wantErr: true,
		},
		{
			name:    "config filter.lfs.clean write denied",
			args:    []string{"config", "filter.lfs.clean", "cat"},
			wantErr: true,
		},
		{
			name:    "config --global denied",
			args:    []string{"config", "--global", "user.name", "Foo"},
			wantErr: true,
		},
		{
			name:    "config --system denied",
			args:    []string{"config", "--system", "user.name", "Foo"},
			wantErr: true,
		},
		{
			name:    "config credential.helper write denied",
			args:    []string{"config", "credential.helper", "store"},
			wantErr: true,
		},
		{
			name:    "config include.path write denied",
			args:    []string{"config", "include.path", "/evil"},
			wantErr: true,
		},
		{
			name:    "deny submodule",
			args:    []string{"submodule", "add", "https://evil"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			invocation, err := classifyGitInvocation(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("classifyGitInvocation: %v", err)
			}
			if invocation.mode != tt.wantMode {
				t.Fatalf("mode = %v, want %v", invocation.mode, tt.wantMode)
			}
			if tt.wantMode != gitInvocationBrokered {
				return
			}
			if invocation.request == nil {
				t.Fatal("expected brokered request")
			}
			if invocation.request.Op != tt.wantOp {
				t.Fatalf("op = %q, want %q", invocation.request.Op, tt.wantOp)
			}
			if invocation.request.Remote != tt.wantRemote {
				t.Fatalf("remote = %q, want %q", invocation.request.Remote, tt.wantRemote)
			}
		})
	}
}
