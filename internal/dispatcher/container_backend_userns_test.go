package dispatcher

import (
	"context"
	"testing"

	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// container_backend_userns_test.go pins the rootless-podman uid-mapping fix
// found by the 2026-07-26 volume-only dogfood (docs/plans/
// volume-only-daemon.md §論点 i's "Podman promoted to primary target").
//
// The failure it closes: under ROOTLESS podman, a container started without
// an explicit userns mode gets a fresh user namespace whose in-container uid
// 1000 maps to a host SUBUID (~100999), not to the host user running podman.
// Every bind mount the daemon hands a job container is created by the daemon
// itself and therefore owned by the host user — most consequentially the
// per-workspace HOME (internal/dispatcher/home.go, mode 0700) mounted at the
// job's $HOME. An in-container uid that is not that owner cannot even
// traverse it, so the job container died instantly with
//
//	profiles: read "/home/boid/.config/boid/config.yaml": open
//	  /home/boid/.config/boid/config.yaml: permission denied
//
// on the very first `boid exec` of the dogfood — reproduced directly with
// `podman run -u 1000:1000 -v <workspace-home>:/home/boid` (permission
// denied) versus the same command plus `--userns keep-id` (reads fine).
// podman's docker-compat API accepts HostConfig.UsernsMode "keep-id" (also
// verified directly against /v1.41/containers/create), and the moby client
// SDK performs no client-side validation of that field, so it reaches the
// engine verbatim.
//
// Deliberately narrow: "keep-id" is a podman-only value that podman itself
// REFUSES for containers created by root, so it is emitted only when the
// engine is both podman AND rootless. Docker (rootful or rootless) keeps
// byte-for-byte today's behavior — an unset UsernsMode — which is what the
// e2e-container CI job exercises on a real docker engine.

func TestUsernsModeForEngine(t *testing.T) {
	podmanVersion := client.ServerVersionResult{
		Platform: client.PlatformInfo{Name: "linux/amd64/ubuntu-24.04"},
		Components: []system.ComponentVersion{
			{Name: "Podman Engine", Version: "4.9.3"},
			{Name: "Conmon", Version: "conmon version 2.1.10"},
		},
	}
	dockerVersion := client.ServerVersionResult{
		Platform: client.PlatformInfo{Name: "Docker Engine - Community"},
		Components: []system.ComponentVersion{
			{Name: "Engine", Version: "27.3.1"},
		},
	}
	rootless := client.SystemInfoResult{Info: system.Info{
		SecurityOptions: []string{"name=apparmor", "name=seccomp,profile=default", "name=rootless"},
	}}
	rootful := client.SystemInfoResult{Info: system.Info{
		SecurityOptions: []string{"name=apparmor", "name=seccomp,profile=default"},
	}}

	tests := []struct {
		name    string
		version client.ServerVersionResult
		info    client.SystemInfoResult
		want    string
	}{
		{
			// The dogfood host: podman 4.9.3 rootless. The only
			// combination that gets keep-id.
			name:    "podman rootless gets keep-id",
			version: podmanVersion,
			info:    rootless,
			want:    "keep-id",
		},
		{
			// podman itself rejects keep-id for root-created
			// containers, so emitting it here would turn a working
			// deploy into a hard container-create failure.
			name:    "podman rootful stays unset",
			version: podmanVersion,
			info:    rootful,
			want:    "",
		},
		{
			// The e2e-container CI job's engine — must stay
			// byte-for-byte unchanged.
			name:    "docker rootful stays unset",
			version: dockerVersion,
			info:    rootful,
			want:    "",
		},
		{
			// Rootless docker already maps the invoking user to the
			// in-container uid through its own uid-mapping setup and
			// has no keep-id value at all; sending one would be
			// rejected as an invalid userns mode.
			name:    "docker rootless stays unset",
			version: dockerVersion,
			info:    rootless,
			want:    "",
		},
		{
			// Defense in depth for an engine that reports its
			// identity only through Platform.Name (the Components
			// array is documented as informational and not part of
			// the API contract).
			name: "podman identified by platform name only",
			version: client.ServerVersionResult{
				Platform: client.PlatformInfo{Name: "Podman Engine - rootless"},
			},
			info: rootless,
			want: "keep-id",
		},
		{
			// A probe that failed entirely (both calls errored, so
			// both results are zero values) must not silently opt a
			// docker host into a podman-only flag.
			name:    "zero values stay unset",
			version: client.ServerVersionResult{},
			info:    client.SystemInfoResult{},
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := usernsModeForEngine(tt.version, tt.info)
			if string(got) != tt.want {
				t.Errorf("usernsModeForEngine() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestContainerBackend_Launch_RootlessPodman_SetsKeepIDUserns is the wiring
// half: the detection above has to actually reach the ContainerCreate call,
// or a job container still comes up with the wrong uid mapping.
func TestContainerBackend_Launch_RootlessPodman_SetsKeepIDUserns(t *testing.T) {
	api := &fakeDockerAPI{
		ServerVersionFunc: func(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error) {
			return client.ServerVersionResult{
				Components: []system.ComponentVersion{{Name: "Podman Engine", Version: "4.9.3"}},
			}, nil
		},
		InfoFunc: func(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
			return client.SystemInfoResult{Info: system.Info{
				SecurityOptions: []string{"name=rootless"},
			}}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1"})

	hostCfg := api.createCalls[0].HostConfig
	if hostCfg == nil {
		t.Fatal("ContainerCreate HostConfig is nil")
	}
	if string(hostCfg.UsernsMode) != "keep-id" {
		t.Errorf("HostConfig.UsernsMode = %q, want %q on rootless podman", hostCfg.UsernsMode, "keep-id")
	}
}

// TestContainerBackend_Launch_Docker_LeavesUsernsUnset pins the
// no-regression half: every non-podman deploy — including the e2e-container
// CI job and every other test in this package, which leave the fake's
// version/info funcs nil — must keep producing a ContainerCreate with no
// userns mode at all.
func TestContainerBackend_Launch_Docker_LeavesUsernsUnset(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1"})

	hostCfg := api.createCalls[0].HostConfig
	if hostCfg == nil {
		t.Fatal("ContainerCreate HostConfig is nil")
	}
	if hostCfg.UsernsMode != "" {
		t.Errorf("HostConfig.UsernsMode = %q, want empty on a non-podman engine", hostCfg.UsernsMode)
	}
}

// TestContainerBackend_Launch_EngineProbeOncePerBackend pins that the
// engine identity is resolved once and cached, not re-probed on every
// Launch: the probe is two extra round trips to the engine socket, and a
// job dispatch is already latency-sensitive enough without them.
func TestContainerBackend_Launch_EngineProbeOncePerBackend(t *testing.T) {
	var versionCalls, infoCalls int
	api := &fakeDockerAPI{
		ServerVersionFunc: func(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error) {
			versionCalls++
			return client.ServerVersionResult{
				Components: []system.ComponentVersion{{Name: "Podman Engine"}},
			}, nil
		},
		InfoFunc: func(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
			infoCalls++
			return client.SystemInfoResult{Info: system.Info{SecurityOptions: []string{"name=rootless"}}}, nil
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1"})
	mustLaunch(t, be, sandbox.Spec{ID: "job-2", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-2"})

	if versionCalls != 1 {
		t.Errorf("ServerVersion calls = %d, want 1 (cached after the first Launch)", versionCalls)
	}
	if infoCalls != 1 {
		t.Errorf("Info calls = %d, want 1 (cached after the first Launch)", infoCalls)
	}
	for i, call := range api.createCalls {
		if string(call.HostConfig.UsernsMode) != "keep-id" {
			t.Errorf("create call %d UsernsMode = %q, want keep-id", i, call.HostConfig.UsernsMode)
		}
	}
}

// TestContainerBackend_Launch_EngineProbeFailure_LeavesUsernsUnset pins the
// failure posture: an engine that cannot answer /version or /info at all
// must not block dispatch. Falling back to "unset" keeps the pre-fix
// behavior, which is correct for every engine except rootless podman —
// where the job would have failed on the uid mapping anyway, with its own
// far more specific permission-denied error.
func TestContainerBackend_Launch_EngineProbeFailure_LeavesUsernsUnset(t *testing.T) {
	api := &fakeDockerAPI{
		ServerVersionFunc: func(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error) {
			return client.ServerVersionResult{}, context.DeadlineExceeded
		},
		InfoFunc: func(context.Context, client.InfoOptions) (client.SystemInfoResult, error) {
			return client.SystemInfoResult{}, context.DeadlineExceeded
		},
	}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1"})

	if got := api.createCalls[0].HostConfig.UsernsMode; got != "" {
		t.Errorf("HostConfig.UsernsMode = %q, want empty when the engine probe fails", got)
	}
}
