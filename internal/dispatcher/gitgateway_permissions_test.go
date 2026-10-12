package dispatcher

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestBuildGatewayRepos_NoPrimaryNeverGrantsPush(t *testing.T) {
	r := &Runner{Projects: fakeProjectLookup{projects: []*orchestrator.Project{
		{ID: "peer", WorkspaceID: "ws", Perm: "push", UpstreamURL: "https://github.com/org/peer.git"},
	}}, Workspaces: fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{
		"ws": {ExtraRepos: []string{"https://github.com/org/extra.git"}},
	}}}
	repos := r.buildGatewayRepos(&orchestrator.JobSpec{Visibility: orchestrator.Visibility{Writable: true}}, "ws")
	if len(repos) != 2 {
		t.Fatalf("repos = %v", repos)
	}
	for key, perm := range repos {
		if perm != gitgateway.PermFetch {
			t.Fatalf("%s has permission %v", key, perm)
		}
	}
}

func TestBuildGatewayRepos_ReceivePackPermissions(t *testing.T) {
	for _, tc := range []struct {
		name, perm, primary string
		writable            bool
		want                int
	}{
		{"writable push primary", "push", "primary", true, 200},
		{"readonly push primary", "push", "primary", false, 403},
		{"writable fetch primary", "fetch", "primary", true, 403},
		{"no primary", "push", "", true, 403},
		{"unspecified permission", "", "primary", true, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { w.WriteHeader(http.StatusOK) }))
			defer upstream.Close()
			u, _ := url.Parse(upstream.URL)
			r := &Runner{Projects: fakeProjectLookup{projects: []*orchestrator.Project{
				{ID: "primary", WorkspaceID: "ws", Perm: tc.perm, UpstreamURL: upstream.URL + "/org/primary.git"},
				{ID: "peer", WorkspaceID: "ws", Perm: "push", UpstreamURL: upstream.URL + "/org/peer.git"},
				{ID: "outside", WorkspaceID: "other", Perm: "push", UpstreamURL: upstream.URL + "/org/outside.git"},
			}}, Workspaces: fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{
				"ws": {ExtraRepos: []string{upstream.URL + "/org/extra.git"}},
			}}}
			spec := &orchestrator.JobSpec{ProjectID: tc.primary, Visibility: orchestrator.Visibility{Writable: tc.writable}}
			reg := gitgateway.NewRegistry()
			token := reg.Register(r.buildGatewayRepos(spec, "ws"), "ws")
			creds := gitgateway.NewCredentialProvider([]gitgateway.HostForgeConfig{{Host: u.Host, Forge: gitgateway.ForgeGitHub, SecretKey: "pat", Scheme: "http"}}, func(namespace, key string) (string, error) { return "test-pat", nil })
			gw := httptest.NewServer(gitgateway.NewServer(reg, creds, nil))
			defer gw.Close()
			for _, repo := range []string{"primary", "peer", "extra", "outside"} {
				want := http.StatusForbidden
				if repo == "primary" {
					want = tc.want
				}
				path := fmt.Sprintf("%s/j/%s/%s/org/%s.git/", gw.URL, token, u.Host, repo)
				for _, advertise := range []bool{false, true} {
					method, endpoint := http.MethodPost, "git-receive-pack"
					if advertise {
						method, endpoint = http.MethodGet, "info/refs?service=git-receive-pack"
					}
					req, _ := http.NewRequest(method, path+endpoint, strings.NewReader("0000"))
					resp, err := http.DefaultClient.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					if resp.StatusCode != want {
						t.Errorf("%s %s: status %d, want %d", method, repo, resp.StatusCode, want)
					}
				}
			}
		})
	}
}

func TestDispatch_GatewayUsesJobWorkspace(t *testing.T) {
	d := newGatewayTestDB(t)
	workspaces := orchestrator.NewWorkspaceRepository(d.Conn)
	for _, slug := range []string{"original", "current"} {
		if err := workspaces.Save(slug, &orchestrator.WorkspaceMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []*orchestrator.Project{
		{ID: "primary", WorkDir: "/tmp", UpstreamURL: "https://github.com/org/primary.git"},
		{ID: "original-peer", WorkDir: "/tmp", UpstreamURL: "https://github.com/org/original-peer.git"},
		{ID: "current-peer", WorkDir: "/tmp", UpstreamURL: "https://github.com/org/current-peer.git"},
	} {
		if err := orchestrator.CreateProject(d.Conn, p); err != nil {
			t.Fatal(err)
		}
		workspace := "original"
		if p.ID == "current-peer" {
			workspace = "current"
		}
		if err := orchestrator.SetProjectWorkspace(d.Conn, p.ID, workspace); err != nil {
			t.Fatal(err)
		}
	}
	task := &orchestrator.Task{ProjectID: "primary", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}, Title: "task"}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.SetProjectWorkspace(d.Conn, "primary", "current"); err != nil {
		t.Fatal(err)
	}
	registry := gitgateway.NewRegistry()
	gatewayURL := "http://10.0.2.2:9"
	r := &Runner{DB: d.Conn, Projects: orchestrator.DBProjectCatalog{DB: d.Conn}, Backend: &gwFakeBackend{}, BoidBinary: "/boid", GitGateway: registry, GatewayURL: &gatewayURL}
	jobID, err := r.Dispatch(context.Background(), &orchestrator.JobSpec{ProjectID: "primary", TaskID: task.ID, Argv: []string{"echo", "hi"}, Kind: orchestrator.JobKindHook, Visibility: orchestrator.Visibility{Writable: true}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.UnregisterJob(jobID)
	entry, ok := registry.Lookup(r.gatewayTokens[jobID])
	if !ok {
		t.Fatal("gateway token missing")
	}
	if entry.Repos[gitgateway.NewRepoKey("github.com", "org", "original-peer")] != gitgateway.PermFetch {
		t.Fatal("job workspace peer missing", entry.Repos)
	}
	if _, ok := entry.Repos[gitgateway.NewRepoKey("github.com", "org", "current-peer")]; ok {
		t.Fatal("current project workspace used instead of job ownership", entry.Repos)
	}
}
