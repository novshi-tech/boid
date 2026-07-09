package gitgateway

import "testing"

func TestParsePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    route
		wantErr bool
	}{
		{
			name: "info/refs with .git suffix",
			path: "/j/tok123/github.com/owner/repo.git/info/refs",
			want: route{token: "tok123", host: "github.com", owner: "owner", repo: "repo", endpoint: "info/refs"},
		},
		{
			name: "info/refs without .git suffix",
			path: "/j/tok123/github.com/owner/repo/info/refs",
			want: route{token: "tok123", host: "github.com", owner: "owner", repo: "repo", endpoint: "info/refs"},
		},
		{
			name: "git-upload-pack with .git suffix",
			path: "/j/tok123/github.com/owner/repo.git/git-upload-pack",
			want: route{token: "tok123", host: "github.com", owner: "owner", repo: "repo", endpoint: "git-upload-pack"},
		},
		{
			name: "git-receive-pack without .git suffix",
			path: "/j/tok123/bitbucket.org/team/repo/git-receive-pack",
			want: route{token: "tok123", host: "bitbucket.org", owner: "team", repo: "repo", endpoint: "git-receive-pack"},
		},
		{
			name: "repo name containing a dot, with .git suffix",
			path: "/j/tok123/github.com/owner/my.repo.git/info/refs",
			want: route{token: "tok123", host: "github.com", owner: "owner", repo: "my.repo", endpoint: "info/refs"},
		},
		{
			name: "repo name containing a dot, without .git suffix",
			path: "/j/tok123/github.com/owner/my.repo/info/refs",
			want: route{token: "tok123", host: "github.com", owner: "owner", repo: "my.repo", endpoint: "info/refs"},
		},
		{
			name: "host carrying a port (test upstream)",
			path: "/j/tok123/127.0.0.1:54321/owner/repo.git/git-upload-pack",
			want: route{token: "tok123", host: "127.0.0.1:54321", owner: "owner", repo: "repo", endpoint: "git-upload-pack"},
		},
		{
			name:    "missing prefix",
			path:    "/github.com/owner/repo.git/info/refs",
			wantErr: true,
		},
		{
			name:    "missing token",
			path:    "/j//github.com/owner/repo.git/info/refs",
			wantErr: true,
		},
		{
			name:    "missing owner segment",
			path:    "/j/tok123/github.com/repo.git/info/refs",
			wantErr: true,
		},
		{
			name:    "unrecognized endpoint",
			path:    "/j/tok123/github.com/owner/repo.git/HEAD",
			wantErr: true,
		},
		{
			name:    "dumb protocol path (objects) not supported",
			path:    "/j/tok123/github.com/owner/repo.git/objects/info/packs",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parsePath(tt.path)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parsePath(%q) = %+v, want error", tt.path, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePath(%q) unexpected error: %v", tt.path, err)
			}
			if got != tt.want {
				t.Fatalf("parsePath(%q) = %+v, want %+v", tt.path, got, tt.want)
			}
		})
	}
}

func TestRepoKeyNormalizesGitSuffix(t *testing.T) {
	withSuffix := NewRepoKey("github.com", "owner", "repo.git")
	withoutSuffix := NewRepoKey("github.com", "owner", "repo")
	if withSuffix != withoutSuffix {
		t.Fatalf("RepoKey suffix mismatch: %q != %q", withSuffix, withoutSuffix)
	}
	if withSuffix != "github.com/owner/repo" {
		t.Fatalf("RepoKey = %q, want %q", withSuffix, "github.com/owner/repo")
	}
}

func TestRouteRepoKeyMatchesBothSuffixForms(t *testing.T) {
	withSuffix, err := parsePath("/j/t/github.com/owner/repo.git/info/refs")
	if err != nil {
		t.Fatal(err)
	}
	withoutSuffix, err := parsePath("/j/t/github.com/owner/repo/info/refs")
	if err != nil {
		t.Fatal(err)
	}
	if withSuffix.repoKey() != withoutSuffix.repoKey() {
		t.Fatalf("repoKey mismatch: %q != %q", withSuffix.repoKey(), withoutSuffix.repoKey())
	}
}

func TestMethodForEndpoint(t *testing.T) {
	if got := methodForEndpoint(EndpointInfoRefs); got != "GET" {
		t.Fatalf("methodForEndpoint(info/refs) = %q, want GET", got)
	}
	for _, ep := range []string{EndpointUploadPack, EndpointReceivePack} {
		if got := methodForEndpoint(ep); got != "POST" {
			t.Fatalf("methodForEndpoint(%s) = %q, want POST", ep, got)
		}
	}
}

func TestOperationForEndpoint(t *testing.T) {
	tests := []struct {
		endpoint string
		service  string
		want     Operation
		wantErr  bool
	}{
		{endpoint: EndpointUploadPack, want: OpFetch},
		{endpoint: EndpointReceivePack, want: OpPush},
		{endpoint: EndpointInfoRefs, service: "git-upload-pack", want: OpFetch},
		{endpoint: EndpointInfoRefs, service: "git-receive-pack", want: OpPush},
		{endpoint: EndpointInfoRefs, service: "", wantErr: true},
		{endpoint: EndpointInfoRefs, service: "something-else", wantErr: true},
	}
	for _, tt := range tests {
		got, err := operationForEndpoint(tt.endpoint, tt.service)
		if tt.wantErr {
			if err == nil {
				t.Fatalf("operationForEndpoint(%q, %q) = %v, want error", tt.endpoint, tt.service, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("operationForEndpoint(%q, %q) unexpected error: %v", tt.endpoint, tt.service, err)
		}
		if got != tt.want {
			t.Fatalf("operationForEndpoint(%q, %q) = %v, want %v", tt.endpoint, tt.service, got, tt.want)
		}
	}
}
