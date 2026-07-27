package dispatcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/volume"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/dockerres"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
	"github.com/novshi-tech/boid/internal/sandbox/dockerproxy"
)

// This file covers PR1 of docs/plans/workspace-home-volume-persistence.md:
// the containment that must be in place BEFORE any workspace HOME volume
// exists, so the volume PR6 introduces is not destroyed by one of the four
// pre-existing sweep paths the moment a daemon restarts.

// TestReapOrphanVolumes_PreservesWorkspaceHomeByName pins 経路 2's first
// guard: a volume whose NAME is in the workspace HOME namespace is never
// passed to VolumeRemove, even when it is otherwise a fair reap target
// (matching install_id).
func TestReapOrphanVolumes_PreservesWorkspaceHomeByName(t *testing.T) {
	api := &fakeDockerAPI{
		VolumeListFunc: func(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error) {
			return client.VolumeListResult{Items: []volume.Volume{
				{Name: "boid-ws-home-install1-default", Labels: map[string]string{labelInstallID: "install-A"}},
				{Name: "job-scratch-vol", Labels: map[string]string{labelJobID: "job-a", labelInstallID: "install-A"}},
			}}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-A"})

	if _, err := be.ReapOrphans(context.Background()); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}

	if got := api.volumeRemoveIDsSnapshot(); len(got) != 1 || got[0] != "job-scratch-vol" {
		t.Errorf("VolumeRemove calls = %v, want exactly [job-scratch-vol]", got)
	}
}

// TestReapOrphanVolumes_PreservesWorkspaceHomeByLabel pins 経路 2's second,
// independent guard: even a volume whose name does NOT look like a workspace
// HOME is preserved when it carries boid.workspace_home. Defense in depth —
// either signal alone is enough, so a future naming change cannot silently
// re-open the hole.
func TestReapOrphanVolumes_PreservesWorkspaceHomeByLabel(t *testing.T) {
	api := &fakeDockerAPI{
		VolumeListFunc: func(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error) {
			return client.VolumeListResult{Items: []volume.Volume{
				{Name: "legacy-named-home", Labels: map[string]string{
					labelInstallID:               "install-A",
					dockerres.LabelWorkspaceHome: "default",
				}},
				{Name: "job-scratch-vol", Labels: map[string]string{labelJobID: "job-a", labelInstallID: "install-A"}},
			}}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-A"})

	if _, err := be.ReapOrphans(context.Background()); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}

	if got := api.volumeRemoveIDsSnapshot(); len(got) != 1 || got[0] != "job-scratch-vol" {
		t.Errorf("VolumeRemove calls = %v, want exactly [job-scratch-vol]", got)
	}
}

// TestReapOrphanVolumes_PreservesWorkspaceHomeByEmptyValuedLabel pins that
// the label guard is a PRESENCE check, matching what reapOrphanVolumes's doc
// comment promises ("the boid.workspace_home label is present"). An
// empty-valued label is still a present label — it is what
// ensureNamedVolumes emits for a volume whose workspace slug is empty (the
// DI/test wiring that never sets one) — and the pre-fix `!= ""` value check
// silently dropped exactly that case out of the protected set (PR1 codex
// review, Minor).
func TestReapOrphanVolumes_PreservesWorkspaceHomeByEmptyValuedLabel(t *testing.T) {
	api := &fakeDockerAPI{
		VolumeListFunc: func(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error) {
			return client.VolumeListResult{Items: []volume.Volume{
				{Name: "legacy-named-home", Labels: map[string]string{
					labelInstallID:               "install-A",
					dockerres.LabelWorkspaceHome: "",
				}},
				{Name: "job-scratch-vol", Labels: map[string]string{labelJobID: "job-a", labelInstallID: "install-A"}},
			}}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-A"})

	if _, err := be.ReapOrphans(context.Background()); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}

	if got := api.volumeRemoveIDsSnapshot(); len(got) != 1 || got[0] != "job-scratch-vol" {
		t.Errorf("VolumeRemove calls = %v, want exactly [job-scratch-vol] — an empty-valued boid.workspace_home label is still present", got)
	}
}

// TestReapOrphanVolumes_StillReapsWorkspaceNetworkNamedVolume guards against
// over-containment: "boid-ws-" (the reserved namespace, shared with the
// per-workspace NETWORK names) is deliberately NOT the skip key —
// "boid-ws-home-" is. A reserved-but-not-HOME volume stays reapable.
func TestReapOrphanVolumes_StillReapsWorkspaceNetworkNamedVolume(t *testing.T) {
	api := &fakeDockerAPI{
		VolumeListFunc: func(ctx context.Context, options client.VolumeListOptions) (client.VolumeListResult, error) {
			return client.VolumeListResult{Items: []volume.Volume{
				{Name: "boid-ws-install1-default", Labels: map[string]string{labelJobID: "job-a", labelInstallID: "install-A"}},
			}}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-A"})

	if _, err := be.ReapOrphans(context.Background()); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}
	if got := api.volumeRemoveIDsSnapshot(); len(got) != 1 || got[0] != "boid-ws-install1-default" {
		t.Errorf("VolumeRemove calls = %v, want [boid-ws-install1-default] (reserved but regenerable)", got)
	}
}

// TestReapOrphans_LedgerUnionPassPreservesWorkspaceHomes pins the wiring
// half of 経路 1: ReapOrphans's internal reap.Run pass (startup reap) must
// hand it the preserving policy. A ledger entry is the only way a
// boid-ws-home-* name reaches that pass without a label, so it is also the
// case a label-based rule could never cover.
func TestReapOrphans_LedgerUnionPassPreservesWorkspaceHomes(t *testing.T) {
	runtimeDir := t.TempDir()
	jobDir := filepath.Join(runtimeDir, "job-1")
	if err := os.MkdirAll(jobDir, 0o755); err != nil {
		t.Fatalf("mkdir job dir: %v", err)
	}
	ledger := dockerproxy.NewLedger(filepath.Join(jobDir, "docker-resources.jsonl"))
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "volume", ID: "boid-ws-home-install1-default"}); err != nil {
		t.Fatalf("append ledger entry: %v", err)
	}
	if err := ledger.Append(dockerproxy.ResourceEntry{Type: "volume", ID: "sibling-scratch"}); err != nil {
		t.Fatalf("append ledger entry: %v", err)
	}

	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-A", RuntimeDir: runtimeDir})

	if _, err := be.ReapOrphans(context.Background()); err != nil {
		t.Fatalf("ReapOrphans: %v", err)
	}

	for _, name := range api.volumeRemoveIDsSnapshot() {
		if dockerres.IsWorkspaceHomeVolumeName(name) {
			t.Errorf("startup reap destroyed workspace HOME volume %q; want it preserved", name)
		}
	}
	if got := api.volumeRemoveIDsSnapshot(); len(got) != 1 || got[0] != "sibling-scratch" {
		t.Errorf("VolumeRemove calls = %v, want exactly [sibling-scratch]", got)
	}
}

// TestEnsureNamedVolumes_WorkspaceHomeGetsOwnLabelsOnly pins 経路 3: a
// workspace HOME volume must carry ONLY boid.workspace_home +
// boid.workspace_home_install_id. Any of the three job labels would put it
// back into an existing sweep's enumeration filter — which is the whole
// failure this PR prevents.
func TestEnsureNamedVolumes_WorkspaceHomeGetsOwnLabelsOnly(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-xyz"})

	homeVol := dockerres.WorkspaceHomeVolumeName("install-xyz", "myworkspace")
	spec := sandbox.Spec{
		ID:   "job-home-vol",
		Argv: []string{"true"},
		Mounts: []sandbox.Mount{
			{Source: homeVol, Target: "/home/boid", Type: sandbox.MountBind},
		},
	}
	mustLaunch(t, be, spec, backend.LaunchOptions{JobID: "job-home-vol", Workspace: "myworkspace", WorkspaceSlug: "myworkspace"})

	if len(api.volumeCreateCalls) != 1 {
		t.Fatalf("VolumeCreate calls = %d, want 1", len(api.volumeCreateCalls))
	}
	call := api.volumeCreateCalls[0]
	if call.Name != homeVol {
		t.Fatalf("VolumeCreate Name = %q, want %q", call.Name, homeVol)
	}

	for _, forbidden := range []string{labelJobID, labelInstallID, labelWorkspace} {
		if v, ok := call.Labels[forbidden]; ok {
			t.Errorf("workspace HOME volume carries %q=%q; it must not (existing reap filters enumerate on it)", forbidden, v)
		}
	}
	if got, want := call.Labels[dockerres.LabelWorkspaceHome], "myworkspace"; got != want {
		t.Errorf("Labels[%q] = %q, want %q", dockerres.LabelWorkspaceHome, got, want)
	}
	if got, want := call.Labels[dockerres.LabelWorkspaceHomeInstallID], "install-xyz"; got != want {
		t.Errorf("Labels[%q] = %q, want %q", dockerres.LabelWorkspaceHomeInstallID, got, want)
	}
}

// TestEnsureNamedVolumes_JobVolumeKeepsJobLabelsAlongsideHomeVolume pins
// that the label split is PER NAME, not per Launch: a job that mounts both
// its own named volume and a workspace HOME volume must get job labels on
// the former and workspace-home labels on the latter.
func TestEnsureNamedVolumes_JobVolumeKeepsJobLabelsAlongsideHomeVolume(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-xyz"})

	homeVol := dockerres.WorkspaceHomeVolumeName("install-xyz", "myworkspace")
	spec := sandbox.Spec{
		ID:   "job-mixed",
		Argv: []string{"true"},
		Mounts: []sandbox.Mount{
			{Source: homeVol, Target: "/home/boid", Type: sandbox.MountBind},
			{Source: "job-cache-vol", Target: "/mnt/cache", Type: sandbox.MountBind},
		},
	}
	mustLaunch(t, be, spec, backend.LaunchOptions{JobID: "job-mixed", Workspace: "myworkspace", WorkspaceSlug: "myworkspace"})

	byName := map[string]map[string]string{}
	for _, c := range api.volumeCreateCalls {
		byName[c.Name] = c.Labels
	}
	if len(byName) != 2 {
		t.Fatalf("VolumeCreate calls = %v, want one per named volume", byName)
	}
	if got := byName["job-cache-vol"][labelJobID]; got != "job-mixed" {
		t.Errorf("job volume Labels[%q] = %q, want %q", labelJobID, got, "job-mixed")
	}
	if got := byName["job-cache-vol"][labelInstallID]; got != "install-xyz" {
		t.Errorf("job volume Labels[%q] = %q, want %q", labelInstallID, got, "install-xyz")
	}
	if _, ok := byName[homeVol][labelJobID]; ok {
		t.Errorf("workspace HOME volume still carries %q", labelJobID)
	}
	if got := byName[homeVol][dockerres.LabelWorkspaceHome]; got != "myworkspace" {
		t.Errorf("home volume Labels[%q] = %q, want %q", dockerres.LabelWorkspaceHome, got, "myworkspace")
	}
}

// TestEnsureNamedVolumes_RejectsInvalidVolumeName pins the guard 論点 e's
// option (i) requires: realization classifies any non-absolute mount Source
// as a named volume, so a stray relative path must fail Launch closed rather
// than reach VolumeCreate.
func TestEnsureNamedVolumes_RejectsInvalidVolumeName(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-xyz"})

	spec := sandbox.Spec{
		ID:   "job-bad-vol",
		Argv: []string{"true"},
		Mounts: []sandbox.Mount{
			{Source: "relative/oops", Target: "/mnt/oops", Type: sandbox.MountBind},
		},
	}
	_, err := be.Launch(context.Background(), spec, backend.LaunchOptions{JobID: "job-bad-vol"})
	if err == nil {
		t.Fatal("Launch succeeded for a mount source that is not a valid docker volume name, want a fail-closed error")
	}
	if !strings.Contains(err.Error(), "relative/oops") {
		t.Errorf("error %q does not name the offending source", err)
	}
	if len(api.volumeCreateCalls) != 0 {
		t.Errorf("VolumeCreate calls = %v, want none (rejected before the engine call)", api.volumeCreateCalls)
	}
	if len(api.createCalls) != 0 {
		t.Errorf("ContainerCreate calls = %d, want 0 (Launch must fail closed)", len(api.createCalls))
	}
}
