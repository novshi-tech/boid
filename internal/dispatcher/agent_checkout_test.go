package dispatcher

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"
	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox/realization"
)

func TestCheckoutCacheWorkspaceIsolation(t *testing.T) {
	for _, kind := range []orchestrator.JobKind{orchestrator.JobKindHook, orchestrator.JobKindExec, orchestrator.JobKindSession} {
		for _, writable := range []bool{true, false} {
			for _, slug := range []string{"team-a", "team-b"} {
				name := dockerres.WorkspaceGitCacheVolumeName("12345678-install", slug)
				spec, err := BuildSandboxSpec(&orchestrator.JobSpec{Kind: kind, Env: map[string]string{"BOID_PRIMARY_REPO": "wrong", "BOID_GIT_BASE": "wrong", "BOID_FORK_POINT": "wrong"}, Visibility: orchestrator.Visibility{Writable: writable}}, SandboxRuntimeInfo{WorkspaceGitCacheVolume: name, PrimaryRepo: "github.com/owner/repo", ForkPoint: "origin/main", GatewayURL: "https://gateway", GatewayJobToken: "token"})
				if err != nil {
					t.Fatal(err)
				}
				for key, want := range map[string]string{"BOID_GIT_CACHE": "/var/cache/boid/git", "BOID_PRIMARY_REPO": "github.com/owner/repo", "BOID_GIT_BASE": "https://gateway/j/token", "BOID_FORK_POINT": "origin/main"} {
					if spec.Env[key] != want {
						t.Fatalf("%s=%q want %q", key, spec.Env[key], want)
					}
				}
				found := 0
				for _, m := range spec.Mounts {
					if m.Target == "/var/cache/boid/git" {
						found++
						if m.Source != name || m.ReadOnly {
							t.Fatalf("cache mount: %+v", m)
						}
					}
				}
				realized, err := realization.Realize(spec)
				if err != nil {
					t.Fatal(err)
				}
				translated, names := containerMounts(realized)
				cacheCount := 0
				for _, m := range translated {
					if m.Target == "/var/cache/boid/git" {
						cacheCount++
						if m.Type != mount.TypeVolume || m.Source != name || m.ReadOnly {
							t.Fatalf("translated mount: %+v", m)
						}
					}
				}
				if cacheCount != 1 {
					t.Fatalf("translated caches=%d; volumes=%v", cacheCount, names)
				}
				if found != 1 {
					t.Fatalf("cache mounts=%d", found)
				}
				if name == dockerres.WorkspaceGitCacheVolumeName("12345678-install", "other") {
					t.Fatal("workspaces share cache")
				}
			}
		}
	}
}

func TestGitCacheVolumeHasPersistentLabels(t *testing.T) {
	api := &fakeDockerAPI{}
	b := &containerBackend{api: api, installID: "install-123"}
	name := dockerres.WorkspaceGitCacheVolumeName(b.installID, "team")
	if err := b.ensureNamedVolumes(context.Background(), []string{name}, "team", "", map[string]string{dockerres.LabelJobID: "job", dockerres.LabelInstallID: b.installID}); err != nil {
		t.Fatal(err)
	}
	call := api.volumeCreateCalls[0]
	if call.Labels[dockerres.LabelJobID] != "" || call.Labels[dockerres.LabelInstallID] != "" {
		t.Fatalf("ephemeral labels: %v", call.Labels)
	}
	if call.Labels[dockerres.LabelWorkspaceGitCache] != "team" {
		t.Fatalf("cache scope: %v", call.Labels)
	}
}

func TestCheckoutInputsForkPoint(t *testing.T) {
	r := &Runner{Projects: fakeProjectLookup{projects: []*orchestrator.Project{{ID: "self", UpstreamURL: "git@github.com:owner/repo.git"}}}, Workspaces: fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{"team": {ForkPoint: "origin/workspace"}}}}
	for _, tc := range []struct {
		spec *orchestrator.JobSpec
		want string
	}{
		{&orchestrator.JobSpec{ProjectID: "self"}, "origin/workspace"},
		{&orchestrator.JobSpec{ProjectID: "self", Visibility: orchestrator.Visibility{Clone: &orchestrator.CloneDeclaration{BaseBranchForkPoint: "origin/project"}}}, "origin/project"},
		{&orchestrator.JobSpec{ProjectID: "self", Env: map[string]string{"BOID_FORK_POINT": "origin/hook"}}, "origin/hook"},
	} {
		repo, fork := r.checkoutInputs(tc.spec, "team")
		if repo != "github.com/owner/repo" || fork != tc.want {
			t.Fatalf("inputs = %q %q, want %q", repo, fork, tc.want)
		}
	}
	repo, _ := r.checkoutInputs(&orchestrator.JobSpec{}, "team")
	if repo != "" {
		t.Fatalf("workspace-only primary=%q", repo)
	}
}

func TestGitCacheSurvivesOrphanVolumeSweep(t *testing.T) {
	name := dockerres.WorkspaceGitCacheVolumeName("install-a", "team")
	api := &fakeDockerAPI{VolumeListFunc: func(context.Context, client.VolumeListOptions) (client.VolumeListResult, error) {
		return client.VolumeListResult{Items: []volume.Volume{{Name: name, Labels: map[string]string{dockerres.LabelJobID: "dead-job", dockerres.LabelInstallID: "install-a"}}}}, nil
	}}
	b := &containerBackend{api: api, installID: "install-a"}
	b.reapOrphanVolumes(context.Background(), nil)
	if len(api.volumeRemoveIDsSnapshot()) != 0 {
		t.Fatalf("workspace cache removed: %v", api.volumeRemoveIDsSnapshot())
	}
}
