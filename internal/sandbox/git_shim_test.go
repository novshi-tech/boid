package sandbox

import (
	"reflect"
	"testing"
)

func TestClassifyGitInvocation(t *testing.T) {
	tests := []struct {
		name            string
		args            []string
		wantMode        gitInvocationMode
		wantOp          GitOp
		wantRemote      string
		wantRefspecs    []string
		wantSource      string
		wantDest        string
		wantSetUpstream bool
		wantErr         bool
	}{
		{
			name:     "direct status",
			args:     []string{"status", "--short"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "direct no args",
			args:     nil,
			wantMode: gitInvocationDirect,
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
			name:     "direct worktree list",
			args:     []string{"worktree", "list"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "direct merge-tree",
			args:     []string{"merge-tree", "--write-tree", "HEAD", "MERGE_HEAD"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "direct update-ref",
			args:     []string{"update-ref", "refs/heads/main", "abc123"},
			wantMode: gitInvocationDirect,
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
			wantMode: gitInvocationDirect,
		},
		{
			name:     "config --list allowed",
			args:     []string{"config", "--list"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "config user.name write allowed",
			args:     []string{"config", "user.name", "Foo"},
			wantMode: gitInvocationDirect,
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
		{
			name:    "deny clone network",
			args:    []string{"clone", "https://evil"},
			wantErr: true,
		},
		{
			name:    "deny clone without --local",
			args:    []string{"clone", "/tmp/peer", "/tmp/dest"},
			wantErr: true,
		},
		{
			name:       "brokered clone --local",
			args:       []string{"clone", "--local", "/tmp/peer", "/workspace/dest"},
			wantMode:   gitInvocationBrokered,
			wantOp:     GitOpCloneLocal,
			wantSource: "/tmp/peer",
			wantDest:   "/workspace/dest",
		},
		{
			name:    "deny clone --local with dangerous -c flag",
			args:    []string{"clone", "--local", "-c", "core.hooksPath=/tmp/evil", "/tmp/peer", "/workspace/dest"},
			wantErr: true,
		},
		{
			name:    "deny clone --local with --upload-pack",
			args:    []string{"clone", "--local", "--upload-pack=/evil", "/tmp/peer", "/workspace/dest"},
			wantErr: true,
		},
		{
			name:    "deny clone --local with --template",
			args:    []string{"clone", "--local", "--template=/evil", "/tmp/peer", "/workspace/dest"},
			wantErr: true,
		},
		{
			name:    "deny clone --local with --config",
			args:    []string{"clone", "--local", "--config=core.hooksPath=/evil", "/tmp/peer", "/workspace/dest"},
			wantErr: true,
		},
		{
			name:    "deny clone --local with --reference",
			args:    []string{"clone", "--local", "--reference=/tmp/ref", "/tmp/peer", "/workspace/dest"},
			wantErr: true,
		},
		{
			name:    "deny clone --local with --separate-git-dir",
			args:    []string{"clone", "--local", "--separate-git-dir=/tmp/git", "/tmp/peer", "/workspace/dest"},
			wantErr: true,
		},
		{
			name:    "deny remote",
			args:    []string{"remote", "add", "evil", "https://evil"},
			wantErr: true,
		},
		// deny-list 化により以下のサブコマンドが新たに許可される
		{
			name:     "allow cherry-pick",
			args:     []string{"cherry-pick", "abc123"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow apply",
			args:     []string{"apply", "--index", "fix.patch"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow format-patch",
			args:     []string{"format-patch", "HEAD~3"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow bisect",
			args:     []string{"bisect", "start"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow shortlog",
			args:     []string{"shortlog", "-sn"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow describe",
			args:     []string{"describe", "--tags"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow reflog",
			args:     []string{"reflog", "show"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow blame",
			args:     []string{"blame", "-L", "1,10", "main.go"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow grep",
			args:     []string{"grep", "TODO"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow notes",
			args:     []string{"notes", "add", "-m", "note"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow am",
			args:     []string{"am", "--signoff", "fix.mbox"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow archive",
			args:     []string{"archive", "--format=tar", "HEAD"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow ls-tree",
			args:     []string{"ls-tree", "-r", "HEAD"},
			wantMode: gitInvocationDirect,
		},
		{
			name:     "allow cat-file",
			args:     []string{"cat-file", "-t", "HEAD"},
			wantMode: gitInvocationDirect,
		},
		// push -u / --set-upstream cases
		{
			name:            "push -u origin",
			args:            []string{"push", "-u", "origin"},
			wantMode:        gitInvocationBrokered,
			wantOp:          GitOpPush,
			wantRemote:      "origin",
			wantSetUpstream: true,
		},
		{
			name:            "push --set-upstream origin HEAD",
			args:            []string{"push", "--set-upstream", "origin", "HEAD"},
			wantMode:        gitInvocationBrokered,
			wantOp:          GitOpPush,
			wantRemote:      "origin",
			wantRefspecs:    []string{"HEAD"},
			wantSetUpstream: true,
		},
		{
			name:            "push -u without remote",
			args:            []string{"push", "-u"},
			wantMode:        gitInvocationBrokered,
			wantOp:          GitOpPush,
			wantSetUpstream: true,
		},
		// push_delete cases
		{
			name:         "push --delete origin feature",
			args:         []string{"push", "--delete", "origin", "feature"},
			wantMode:     gitInvocationBrokered,
			wantOp:       GitOpPushDelete,
			wantRemote:   "origin",
			wantRefspecs: []string{"feature"},
		},
		{
			name:         "push -D origin feature",
			args:         []string{"push", "-D", "origin", "feature"},
			wantMode:     gitInvocationBrokered,
			wantOp:       GitOpPushDelete,
			wantRemote:   "origin",
			wantRefspecs: []string{"feature"},
		},
		{
			name:         "push origin :refs/heads/feature",
			args:         []string{"push", "origin", ":refs/heads/feature"},
			wantMode:     gitInvocationBrokered,
			wantOp:       GitOpPushDelete,
			wantRemote:   "origin",
			wantRefspecs: []string{":refs/heads/feature"},
		},
		{
			name:         "push origin :feature",
			args:         []string{"push", "origin", ":feature"},
			wantMode:     gitInvocationBrokered,
			wantOp:       GitOpPushDelete,
			wantRemote:   "origin",
			wantRefspecs: []string{":feature"},
		},
		{
			name:       "push --delete origin no refspec",
			args:       []string{"push", "--delete", "origin"},
			wantMode:   gitInvocationBrokered,
			wantOp:     GitOpPushDelete,
			wantRemote: "origin",
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
			if tt.wantRefspecs != nil && !reflect.DeepEqual(invocation.request.Refspecs, tt.wantRefspecs) {
				t.Fatalf("refspecs = %v, want %v", invocation.request.Refspecs, tt.wantRefspecs)
			}
			if tt.wantSource != "" && invocation.request.Source != tt.wantSource {
				t.Fatalf("source = %q, want %q", invocation.request.Source, tt.wantSource)
			}
			if tt.wantDest != "" && invocation.request.Dest != tt.wantDest {
				t.Fatalf("dest = %q, want %q", invocation.request.Dest, tt.wantDest)
			}
			if tt.wantSetUpstream && !invocation.request.SetUpstream {
				t.Fatalf("SetUpstream = false, want true")
			}
		})
	}
}
