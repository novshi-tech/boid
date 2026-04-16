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
