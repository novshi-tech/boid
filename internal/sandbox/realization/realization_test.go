package realization

import (
	"reflect"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestRealize pins Realize's translation contract with representative
// sandbox.Spec inputs (docs/plans/phase6-container-backend.md §PR3): the
// three Mount.Source kinds, env/workdir/argv passthrough, tmpfs, and the
// error path.
func TestRealize(t *testing.T) {
	tests := []struct {
		name    string
		spec    sandbox.Spec
		want    Realization
		wantErr string // non-empty: Realize must return an error containing this substring
	}{
		{
			name: "workspace HOME host bind is classified HostPath",
			spec: sandbox.Spec{
				ID: "job-home",
				Mounts: []sandbox.Mount{
					{
						Source: "/home/boid-user/.local/share/boid/workspaces/default/home",
						Target: "/home/boid",
						Type:   sandbox.MountBind,
					},
				},
			},
			want: Realization{
				ID: "job-home",
				Volumes: []VolumeMount{
					{
						Source: MountSource{
							Kind:  MountSourceHostPath,
							Value: "/home/boid-user/.local/share/boid/workspaces/default/home",
						},
						Target: "/home/boid",
					},
				},
			},
		},
		{
			name: "workspace clone target /workspace/<name> lands container-local, not host bind",
			spec: sandbox.Spec{
				ID: "job-clone",
				Mounts: []sandbox.Mount{
					{
						// Mirrors cloneMounts' /workspace bind
						// (internal/dispatcher/sandbox_builder.go): host
						// runtime dir Source, but the container backend
						// must NOT treat this as a host bind (決定 4).
						Source: "/home/boid-user/.local/share/boid/runtimes/job-clone/workspace",
						Target: "/workspace/bm-next",
						Type:   sandbox.MountBind,
					},
				},
			},
			want: Realization{
				ID: "job-clone",
				Volumes: []VolumeMount{
					{
						Source: MountSource{Kind: MountSourceContainerLocal, Value: "/workspace/bm-next"},
						Target: "/workspace/bm-next",
					},
				},
			},
		},
		{
			name: "bare /workspace parent (no leaf name) is also container-local",
			spec: sandbox.Spec{
				Mounts: []sandbox.Mount{
					{
						Source: "/home/boid-user/.local/share/boid/runtimes/job/workspace",
						Target: "/workspace",
						Type:   sandbox.MountBind,
					},
				},
			},
			want: Realization{
				Volumes: []VolumeMount{
					{Source: MountSource{Kind: MountSourceContainerLocal, Value: "/workspace"}, Target: "/workspace"},
				},
			},
		},
		{
			name: "clone reference .git bind keeps Guard/DetectType and is classified HostPath",
			spec: sandbox.Spec{
				Mounts: []sandbox.Mount{
					{
						// Mirrors cloneMounts' self-project reference bind.
						Source:     "/home/nose/src/bm-next/.git",
						Target:     "/mnt/refs/self.git",
						Type:       sandbox.MountBind,
						ReadOnly:   true,
						DetectType: true,
						Guard:      "-e /home/nose/src/bm-next/.git",
					},
				},
			},
			want: Realization{
				Volumes: []VolumeMount{
					{
						Source:     MountSource{Kind: MountSourceHostPath, Value: "/home/nose/src/bm-next/.git"},
						Target:     "/mnt/refs/self.git",
						ReadOnly:   true,
						DetectType: true,
						Guard:      "-e /home/nose/src/bm-next/.git",
					},
				},
			},
		},
		{
			name: "boid binary single-file bind keeps IsFile+ReadOnly, classified HostPath",
			spec: sandbox.Spec{
				Mounts: []sandbox.Mount{
					{
						Source:   "/usr/local/bin/boid",
						Target:   "/run/boid/bin/boid",
						Type:     sandbox.MountBind,
						ReadOnly: true,
						IsFile:   true,
					},
				},
			},
			want: Realization{
				Volumes: []VolumeMount{
					{
						Source:   MountSource{Kind: MountSourceHostPath, Value: "/usr/local/bin/boid"},
						Target:   "/run/boid/bin/boid",
						ReadOnly: true,
						IsFile:   true,
					},
				},
			},
		},
		{
			name: "non-absolute Source is classified as a named volume (Phase 7 forward-compat)",
			spec: sandbox.Spec{
				Mounts: []sandbox.Mount{
					{
						Source: "boid-home-workspace-foo",
						Target: "/home/boid",
						Type:   sandbox.MountRBind,
					},
				},
			},
			want: Realization{
				Volumes: []VolumeMount{
					{
						Source: MountSource{Kind: MountSourceNamedVolume, Value: "boid-home-workspace-foo"},
						Target: "/home/boid",
					},
				},
			},
		},
		{
			name: "mount with no Source at all is container-local",
			spec: sandbox.Spec{
				Mounts: []sandbox.Mount{
					{Target: "/mnt/scratch", Type: sandbox.MountBind},
				},
			},
			want: Realization{
				Volumes: []VolumeMount{
					{Source: MountSource{Kind: MountSourceContainerLocal, Value: "/mnt/scratch"}, Target: "/mnt/scratch"},
				},
			},
		},
		{
			name: "tmpfs mount translates to Tmpfs, not Volumes (no Source to classify)",
			spec: sandbox.Spec{
				Mounts: []sandbox.Mount{
					{Target: "/tmp", Type: sandbox.MountTmpfs},
					{Target: "/home/boid/.boid", Type: sandbox.MountTmpfs, ReadOnly: false},
				},
			},
			want: Realization{
				Tmpfs: []TmpfsMount{
					{Target: "/tmp"},
					{Target: "/home/boid/.boid"},
				},
			},
		},
		{
			name: "env (broker socket + token + gateway-style egress fixture), workdir, argv, TTY pass through unchanged",
			spec: sandbox.Spec{
				ID:      "job-env",
				WorkDir: "/workspace/bm-next",
				Argv:    []string{"/run/boid/bin/boid", "runner-inner-child"},
				TTY:     true,
				Env: map[string]string{
					"HOME":               "/home/boid",
					"BOID_BROKER_SOCKET": "/run/boid/broker.sock",
					"BOID_BROKER_TOKEN":  "tok-abc123",
					"https_proxy":        "http://10.0.2.2:41000",
					"NO_PROXY":           "10.0.2.2,10.0.2.3,localhost,127.0.0.1",
				},
			},
			want: Realization{
				ID:      "job-env",
				Workdir: "/workspace/bm-next",
				Argv:    []string{"/run/boid/bin/boid", "runner-inner-child"},
				TTY:     true,
				Env: map[string]string{
					"HOME":               "/home/boid",
					"BOID_BROKER_SOCKET": "/run/boid/broker.sock",
					"BOID_BROKER_TOKEN":  "tok-abc123",
					"https_proxy":        "http://10.0.2.2:41000",
					"NO_PROXY":           "10.0.2.2,10.0.2.3,localhost,127.0.0.1",
				},
			},
		},
		{
			name: "composite: HOME bind + workspace clone + reference bind + .boid tmpfs overlay + env, all in one Spec",
			spec: sandbox.Spec{
				ID:      "job-composite",
				WorkDir: "/workspace/bm-next",
				Env: map[string]string{
					"BOID_BROKER_TOKEN": "tok-xyz",
				},
				Mounts: []sandbox.Mount{
					{
						Source: "/home/boid-user/.local/share/boid/workspaces/default/home",
						Target: "/home/boid",
						Type:   sandbox.MountBind,
					},
					{
						Target: "/home/boid/.boid",
						Type:   sandbox.MountTmpfs,
					},
					{
						Source:     "/home/nose/src/bm-next/.git",
						Target:     "/mnt/refs/self.git",
						Type:       sandbox.MountBind,
						ReadOnly:   true,
						DetectType: true,
						Guard:      "-e /home/nose/src/bm-next/.git",
					},
					{
						Source: "/home/boid-user/.local/share/boid/runtimes/job-composite/workspace",
						Target: "/workspace/bm-next",
						Type:   sandbox.MountBind,
					},
				},
			},
			want: Realization{
				ID:      "job-composite",
				Workdir: "/workspace/bm-next",
				Env:     map[string]string{"BOID_BROKER_TOKEN": "tok-xyz"},
				Volumes: []VolumeMount{
					{
						Source: MountSource{Kind: MountSourceHostPath, Value: "/home/boid-user/.local/share/boid/workspaces/default/home"},
						Target: "/home/boid",
					},
					{
						Source:     MountSource{Kind: MountSourceHostPath, Value: "/home/nose/src/bm-next/.git"},
						Target:     "/mnt/refs/self.git",
						ReadOnly:   true,
						DetectType: true,
						Guard:      "-e /home/nose/src/bm-next/.git",
					},
					{
						Source: MountSource{Kind: MountSourceContainerLocal, Value: "/workspace/bm-next"},
						Target: "/workspace/bm-next",
					},
				},
				Tmpfs: []TmpfsMount{
					{Target: "/home/boid/.boid"},
				},
			},
		},
		{
			name: "mount with empty Target is rejected",
			spec: sandbox.Spec{
				Mounts: []sandbox.Mount{
					{Source: "/some/host/path", Type: sandbox.MountBind},
				},
			},
			wantErr: "empty Target",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Realize(tt.spec)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Realize() error = nil, want error containing %q", tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Realize() error = %q, want it to contain %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Realize() unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Realize() =\n  %#v\nwant\n  %#v", got, tt.want)
			}
		})
	}
}

// TestRealize_EnvIsCopied asserts the Realization.Env map returned by
// Realize does not alias spec.Env — mutating one must not affect the other
// (Realization is meant to be handed off to a container backend that may
// further mutate its own copy, e.g. adding DOCKER_HOST for dockerproxy).
func TestRealize_EnvIsCopied(t *testing.T) {
	spec := sandbox.Spec{Env: map[string]string{"BOID_BROKER_TOKEN": "tok-1"}}
	got, err := Realize(spec)
	if err != nil {
		t.Fatalf("Realize() unexpected error: %v", err)
	}

	got.Env["BOID_BROKER_TOKEN"] = "mutated"
	if spec.Env["BOID_BROKER_TOKEN"] != "tok-1" {
		t.Fatalf("mutating Realization.Env leaked back into spec.Env: got %q", spec.Env["BOID_BROKER_TOKEN"])
	}
}

// TestRealize_ArgvIsCopied mirrors TestRealize_EnvIsCopied for Argv.
func TestRealize_ArgvIsCopied(t *testing.T) {
	spec := sandbox.Spec{Argv: []string{"boid", "runner-inner-child"}}
	got, err := Realize(spec)
	if err != nil {
		t.Fatalf("Realize() unexpected error: %v", err)
	}

	got.Argv[0] = "mutated"
	if spec.Argv[0] != "boid" {
		t.Fatalf("mutating Realization.Argv leaked back into spec.Argv: got %q", spec.Argv[0])
	}
}

// TestMountSourceKind_String smoke-tests the diagnostic String() method,
// including the default branch for an out-of-range value.
func TestMountSourceKind_String(t *testing.T) {
	cases := map[MountSourceKind]string{
		MountSourceNamedVolume:    "named-volume",
		MountSourceHostPath:       "host-path",
		MountSourceContainerLocal: "container-local",
		MountSourceKind(99):       "MountSourceKind(99)",
	}
	for kind, want := range cases {
		if got := kind.String(); got != want {
			t.Errorf("MountSourceKind(%d).String() = %q, want %q", int(kind), got, want)
		}
	}
}
