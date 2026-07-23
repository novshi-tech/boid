package dispatcher

import (
	"context"
	"fmt"
	"testing"

	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins Phase 6 PR9's workspace network isolation (docs/plans/
// phase6-container-backend.md §決定5's "internal network は workspace 単位で
// 分離する" security invariant): containerBackend.Launch, when given a
// non-empty LaunchOptions.Workspace, must attach the job container to a
// per-workspace docker network (created idempotently) instead of leaving it
// on docker's default network — the gap PR9's own e2e-container job exists
// to close (see build/container/compose.yml's "NOT yet true of this file"
// note and container_backend.go's dockerAPI interface, both of which this
// test exercises against the fake rather than real docker).

// TestContainerBackend_Launch_NoWorkspace_LeavesNetworkingConfigUnset pins
// backward compatibility: every pre-PR9 caller (unit test or otherwise)
// that never sets LaunchOptions.Workspace must see byte-for-byte the same
// ContainerCreate call as before this feature — no NetworkCreate call, no
// NetworkingConfig on the create request. This is what lets this whole
// feature land without touching any of the dozens of pre-existing Launch
// call sites in this package's other test files.
func TestContainerBackend_Launch_NoWorkspace_LeavesNetworkingConfigUnset(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1"})

	if len(api.networkCreateCalls) != 0 {
		t.Fatalf("NetworkCreate calls = %d, want 0 when LaunchOptions.Workspace is empty", len(api.networkCreateCalls))
	}
	if api.createCalls[0].NetworkingConfig != nil {
		t.Fatalf("ContainerCreate NetworkingConfig = %+v, want nil when LaunchOptions.Workspace is empty", api.createCalls[0].NetworkingConfig)
	}
	if hostCfg := api.createCalls[0].HostConfig; hostCfg != nil && hostCfg.NetworkMode != "" {
		t.Errorf("HostConfig.NetworkMode = %q, want empty when LaunchOptions.Workspace is empty", hostCfg.NetworkMode)
	}
}

// TestContainerBackend_Launch_WithWorkspace_CreatesIsolatedNetworkAndAttaches
// pins the core of §決定5: a Workspace-scoped Launch must (a) ensure a
// dedicated docker network exists for that workspace, labeled so `boid
// reap`/ReapOrphans can find it later (§決定6), and (b) attach the job
// container to exactly that network via NetworkingConfig — not the docker
// default bridge every sibling and job container would otherwise land on
// together, defeating the whole isolation point.
func TestContainerBackend_Launch_WithWorkspace_CreatesIsolatedNetworkAndAttaches(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-abc"})

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1", Workspace: "ws-a"})

	if len(api.networkCreateCalls) != 1 {
		t.Fatalf("NetworkCreate calls = %d, want 1", len(api.networkCreateCalls))
	}
	netName := api.networkCreateNames[0]
	if netName == "" {
		t.Fatal("NetworkCreate name is empty")
	}
	createOpts := api.networkCreateCalls[0]
	if !createOpts.Internal {
		t.Error("workspace network NetworkCreateOptions.Internal = false, want true (§決定5: no default route out)")
	}
	if createOpts.Labels[labelWorkspace] != "ws-a" {
		t.Errorf("workspace network labels[%q] = %q, want %q", labelWorkspace, createOpts.Labels[labelWorkspace], "ws-a")
	}
	if createOpts.Labels[labelInstallID] != "install-abc" {
		t.Errorf("workspace network labels[%q] = %q, want %q (so `boid reap`'s install_id-scoped sweep finds it)",
			labelInstallID, createOpts.Labels[labelInstallID], "install-abc")
	}

	netCfg := api.createCalls[0].NetworkingConfig
	if netCfg == nil {
		t.Fatal("ContainerCreate NetworkingConfig is nil, want the workspace network attached")
	}
	if _, ok := netCfg.EndpointsConfig[netName]; !ok {
		t.Errorf("ContainerCreate NetworkingConfig.EndpointsConfig = %v, want an entry for %q", netCfg.EndpointsConfig, netName)
	}

	// HostConfig.NetworkMode must ALSO be set to netName (belt-and-
	// suspenders alongside NetworkingConfig — see Launch's own inline
	// comment): a container left on docker's implicit default bridge in
	// addition to netName would defeat §決定5's isolation invariant.
	hostCfg := api.createCalls[0].HostConfig
	if hostCfg == nil || string(hostCfg.NetworkMode) != netName {
		t.Errorf("HostConfig.NetworkMode = %v, want %q", hostCfg, netName)
	}
}

// TestContainerBackend_Launch_WorkspaceNetworkNameStableAcrossJobs pins that
// two Launches for the SAME workspace compute the SAME network name — the
// property that lets a job's own dockerproxy-created sibling (which the
// runner forces onto this exact name independently, see
// containerWorkspaceNetworkName's own doc comment) land on the network the
// job container itself is already attached to.
func TestContainerBackend_Launch_WorkspaceNetworkNameStableAcrossJobs(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{InstallID: "install-abc"})

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1", Workspace: "ws-a"})
	mustLaunch(t, be, sandbox.Spec{ID: "job-2", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-2", Workspace: "ws-a"})

	if len(api.networkCreateNames) != 2 {
		t.Fatalf("NetworkCreate calls = %d, want 2 (one per Launch)", len(api.networkCreateNames))
	}
	if api.networkCreateNames[0] != api.networkCreateNames[1] {
		t.Errorf("workspace network names differ across Launches for the same workspace: %q vs %q",
			api.networkCreateNames[0], api.networkCreateNames[1])
	}

	// Directly pins the pure helper the runner (SetWorkspaceNetwork wiring)
	// independently calls, so a drift between the two call sites is caught
	// even though this test only exercises containerBackend.
	if got := containerWorkspaceNetworkName("install-abc", "ws-a"); got != api.networkCreateNames[0] {
		t.Errorf("containerWorkspaceNetworkName(...) = %q, want %q (must match Launch's own NetworkCreate name)",
			got, api.networkCreateNames[0])
	}
}

// TestContainerBackend_Launch_WorkspaceNetworkAlreadyExists_Tolerated pins
// idempotency: a second (or concurrent) Launch for a workspace whose
// network another Launch already created must not fail merely because
// docker reports the network name is already taken — NetworkCreate is
// called on every Launch (no first-call/cache distinction), so this is the
// common case, not an edge case.
func TestContainerBackend_Launch_WorkspaceNetworkAlreadyExists_Tolerated(t *testing.T) {
	api := &fakeDockerAPI{
		NetworkCreateFunc: func(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
			return client.NetworkCreateResult{}, fakeConflictError{}
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	sess, err := be.Launch(context.Background(), sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1", Workspace: "ws-a"})
	if err != nil {
		t.Fatalf("Launch: %v, want success (an already-exists NetworkCreate error must be tolerated as idempotent)", err)
	}
	if sess == nil {
		t.Fatal("Launch returned a nil session on the tolerated-conflict path")
	}
}

// TestContainerBackend_Launch_WorkspaceNetworkCreateFails_FailsLaunchClosed
// pins the fail-closed half of §決定5's security invariant: an
// unrecoverable NetworkCreate error (anything that is not "already
// exists") must fail Launch outright, never silently fall back to
// launching the job container unisolated on docker's default network.
func TestContainerBackend_Launch_WorkspaceNetworkCreateFails_FailsLaunchClosed(t *testing.T) {
	wantErr := fmt.Errorf("docker daemon unreachable")
	api := &fakeDockerAPI{
		NetworkCreateFunc: func(ctx context.Context, name string, options client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
			return client.NetworkCreateResult{}, wantErr
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	_, err := be.Launch(context.Background(), sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1", Workspace: "ws-a"})
	if err == nil {
		t.Fatal("Launch succeeded despite an unrecoverable NetworkCreate failure — must fail closed, not launch unisolated (§決定5)")
	}
	if len(api.createCalls) != 0 {
		t.Errorf("ContainerCreate calls = %d, want 0 (Launch must fail before creating the job container)", len(api.createCalls))
	}
}

// TestContainerBackend_Launch_SelfContainerID_ConnectsDaemonWithGatewayEgressAliases
// pins the other half of the wiring a workspace-isolated network needs to
// be usable at all: with the job container (and its dockerproxy-created
// siblings) confined to an `Internal: true` network that has no route out,
// the ONLY way it can still reach the git gateway (mandatory — every
// project-visible dispatch clones, see runner.go's own Visibility.Clone
// comment) or the egress proxy is if the DAEMON's own container also joins
// that same network, under the same DNS aliases job containers already
// resolve on the static `boid_internal` compose network
// (build/container/compose.yml). ContainerBackendOptions.SelfContainerID
// (typically $HOSTNAME inside the daemon's own container — see
// sandboxBackendForConfig's wiring) is what tells containerBackend which
// container to connect.
func TestContainerBackend_Launch_SelfContainerID_ConnectsDaemonWithGatewayEgressAliases(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{SelfContainerID: "daemon-container-id"})

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1", Workspace: "ws-a"})

	if len(api.networkConnectIDs) != 1 {
		t.Fatalf("NetworkConnect calls = %d, want 1", len(api.networkConnectIDs))
	}
	if api.networkConnectIDs[0] != api.networkCreateNames[0] {
		t.Errorf("NetworkConnect target network = %q, want the workspace network %q", api.networkConnectIDs[0], api.networkCreateNames[0])
	}
	connOpts := api.networkConnectCalls[0]
	if connOpts.Container != "daemon-container-id" {
		t.Errorf("NetworkConnect Container = %q, want %q (ContainerBackendOptions.SelfContainerID)", connOpts.Container, "daemon-container-id")
	}
	wantAliases := map[string]bool{"boid-gateway": false, "boid-egress": false}
	if connOpts.EndpointConfig == nil {
		t.Fatal("NetworkConnect EndpointConfig is nil, want gateway/egress aliases")
	}
	for _, a := range connOpts.EndpointConfig.Aliases {
		if _, ok := wantAliases[a]; ok {
			wantAliases[a] = true
		}
	}
	for alias, found := range wantAliases {
		if !found {
			t.Errorf("NetworkConnect EndpointConfig.Aliases = %v, want %q present", connOpts.EndpointConfig.Aliases, alias)
		}
	}
}

// TestContainerBackend_Launch_NoSelfContainerID_SkipsNetworkConnect pins
// that ContainerBackendOptions.SelfContainerID's zero value ("" — every
// pre-PR9 caller, and any non-compose test/DI usage) disables the
// daemon-self-connect step entirely: no NetworkConnect call at all, not a
// call with an empty Container that would error.
func TestContainerBackend_Launch_NoSelfContainerID_SkipsNetworkConnect(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1", Workspace: "ws-a"})

	if len(api.networkConnectIDs) != 0 {
		t.Fatalf("NetworkConnect calls = %d, want 0 when SelfContainerID is unset", len(api.networkConnectIDs))
	}
}

// TestContainerBackend_Launch_SelfConnectFailure_DoesNotFailLaunch pins that
// a NetworkConnect failure (e.g. the daemon container is already connected
// from a prior Launch for this same workspace — the common steady-state
// case, since every Launch attempts the connect) degrades to a warning, not
// a hard Launch failure: unlike NetworkCreate (§決定5's fail-closed
// isolation invariant), a failed self-connect only risks THIS daemon's own
// gateway/egress reachability for jobs on a network it may already be
// connected to under a different code path (or already is via
// boid_internal for the default workspace) — not a job's isolation from
// other workspaces, which NetworkCreate above already guarantees.
func TestContainerBackend_Launch_SelfConnectFailure_DoesNotFailLaunch(t *testing.T) {
	api := &fakeDockerAPI{
		NetworkConnectFunc: func(ctx context.Context, networkID string, options client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
			return client.NetworkConnectResult{}, fmt.Errorf("endpoint already exists")
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{SelfContainerID: "daemon-container-id"})

	sess, err := be.Launch(context.Background(), sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1", Workspace: "ws-a"})
	if err != nil {
		t.Fatalf("Launch: %v, want success despite a NetworkConnect failure", err)
	}
	if sess == nil {
		t.Fatal("Launch returned a nil session")
	}
}

// fakeConflictError is a minimal error satisfying errdefs.IsConflict via the
// containerd/errdefs "classifier interface" mechanism (an error implementing
// Conflict() is recognized without needing to wrap a sentinel) — see
// containerd/errdefs's own resolve.go. Used to simulate docker's real
// "network with name X already exists" 409 response without pulling in an
// HTTP round trip.
type fakeConflictError struct{}

func (fakeConflictError) Error() string { return "network already exists" }
func (fakeConflictError) Conflict()     {}
