package dockerproxy

import (
	"testing"

	"github.com/novshi-tech/boid/internal/dockerres"
)

// This file covers 経路 4 of docs/plans/workspace-home-volume-persistence.md
// 論点 a — the one containment a label design could never provide. A
// sandboxed docker client can name a volume anything, and boid's volume
// names are deterministic and therefore guessable, so the namespace itself
// has to be reserved at the proxy.

func TestVolumesCreate_reservedName_deny(t *testing.T) {
	for _, name := range []string{
		"boid-ws-home-abcd1234-default",
		dockerres.WorkspaceHomeVolumeName("abcd12345678", "default"),
		"boid-ws-abcd1234-default", // whole reserved namespace, not just HOME
		"boid-ws-anything",
	} {
		assertDenyContains(t, "POST", "/volumes/create",
			mustJSON(map[string]interface{}{"Name": name}), "reserved by boid")
	}
}

func TestVolumesCreate_ordinaryName_allow(t *testing.T) {
	for _, name := range []string{"my-volume", "boid-something-else", "boidws", ""} {
		assertAllow(t, "POST", "/volumes/create", mustJSON(map[string]interface{}{"Name": name}))
	}
}

// TestVolumesDelete_reservedName_deny closes the second half of 経路 4: a
// create that is denied still leaves DELETE reachable for a volume the
// sandbox learned the name of some other way (it is deterministic), and
// DELETE /volumes/* used to be an unconditional pass-through.
func TestVolumesDelete_reservedName_deny(t *testing.T) {
	assertDenyContains(t, "DELETE", "/volumes/boid-ws-home-abcd1234-default", nil, "reserved by boid")
	assertDenyContains(t, "DELETE", "/v1.43/volumes/boid-ws-home-abcd1234-default", nil, "reserved by boid")
	assertDenyContains(t, "DELETE", "/volumes/boid-ws-abcd1234-default", nil, "reserved by boid")
}

func TestVolumesDelete_ordinaryName_allow(t *testing.T) {
	assertAllow(t, "DELETE", "/volumes/my-volume", nil)
	assertAllow(t, "DELETE", "/v1.43/volumes/my-volume", nil)
}

// TestVolumesPrune_deny pins the fifth destruction path, the one the plan
// doc's original 4-path enumeration missed. `docker volume prune` destroys
// every volume no container currently references — which is precisely the
// state every OTHER workspace's HOME volume is in during any given job. It
// carries no name to filter on, so the endpoint has to be closed outright.
// This is the only behavior change PR1 makes visible inside the sandbox.
func TestVolumesPrune_deny(t *testing.T) {
	assertDeny(t, "POST", "/volumes/prune", nil)
	assertDeny(t, "POST", "/v1.43/volumes/prune", nil)
	assertDenyContains(t, "POST", "/volumes/prune", nil, "workspace HOME")
}

// TestOtherPrunes_stillAllowed guards the blast radius of the change above:
// only the volume prune is closed. Container/image/network/system prunes are
// untouched.
func TestOtherPrunes_stillAllowed(t *testing.T) {
	assertAllow(t, "POST", "/containers/prune", nil)
	assertAllow(t, "POST", "/images/prune", nil)
	assertAllow(t, "POST", "/networks/prune", nil)
	assertAllow(t, "POST", "/system/prune", nil)
}

// TestNetworks_reservedPrefixNotApplied pins the deliberate asymmetry
// internal/dockerres's package doc describes: "boid-ws-" is also the prefix
// of boid's per-workspace NETWORK names, but the reservation is a
// VOLUME-namespace rule only. Applying it to networks would break sandboxed
// docker clients for no benefit — boid names its own networks, the sandbox
// does not, and a network is regenerable anyway.
func TestNetworks_reservedPrefixNotApplied(t *testing.T) {
	assertAllow(t, "POST", "/networks/create",
		mustJSON(map[string]interface{}{"Name": "boid-ws-abcd1234-default"}))
	assertAllow(t, "DELETE", "/networks/boid-ws-abcd1234-default", nil)
	assertAllow(t, "DELETE", "/networks/boid-ws-home-abcd1234-default", nil)
}

// --- 経路 6 / 7: attaching an EXISTING reserved volume to a new container ---
//
// 経路 4 above reserves the volume NAMESPACE at the create/delete endpoints,
// but a volume's contents can be destroyed without ever creating or deleting
// the volume: mount it into a throwaway container and `rm -rf` the mountpoint.
// The volume object survives; the harness credentials and the toolchain inside
// it do not. checkContainersCreate inspected a mount's VolumeOptions but not
// its Source, so this whole class was allowed (PR1 codex review Blocker 2).

// TestContainersCreate_mountSourceReservedVolume_deny pins 経路 6. The exact
// request shape codex demonstrated:
//
//	{"Image":"busybox","Cmd":["sh","-c","rm -rf /victim/* /victim/.[!.]*"],
//	 "HostConfig":{"Mounts":[{"Type":"volume",
//	                          "Source":"boid-ws-home-abcd1234-default",
//	                          "Target":"/victim"}]}}
func TestContainersCreate_mountSourceReservedVolume_deny(t *testing.T) {
	for _, name := range []string{
		"boid-ws-home-abcd1234-default",
		dockerres.WorkspaceHomeVolumeName("abcd12345678", "default"),
		"boid-ws-abcd1234-default", // whole reserved namespace, as with create/delete
	} {
		body := createBody(map[string]interface{}{
			"Mounts": []map[string]interface{}{
				{"Type": "volume", "Source": name, "Target": "/victim"},
			},
		})
		assertDenyContains(t, "POST", "/containers/create", body, "reserved by boid")
	}
}

// TestContainersCreate_mountSourceReservedVolume_typeIndependent pins the
// deliberate choice to check Source WITHOUT first agreeing with the engine on
// what Type means — see mountSpec.Source's doc comment. An omitted Type, a
// differently-cased one, or one this policy has never heard of must not be a
// way through.
func TestContainersCreate_mountSourceReservedVolume_typeIndependent(t *testing.T) {
	for _, typ := range []string{"", "volume", "Volume", "VOLUME", "cluster", "npipe"} {
		body := createBody(map[string]interface{}{
			"Mounts": []map[string]interface{}{
				{"Type": typ, "Source": "boid-ws-home-abcd1234-default", "Target": "/victim"},
			},
		})
		assertDenyContains(t, "POST", "/containers/create", body, "reserved by boid")
	}
}

// TestContainersCreate_mountSourceOrdinaryVolume_allow guards the blast
// radius: a sandboxed client mounting its OWN named volume is the normal,
// supported case (TestContainers and friends do it constantly) and must stay
// allowed.
func TestContainersCreate_mountSourceOrdinaryVolume_allow(t *testing.T) {
	for _, name := range []string{"my-volume", "boid-something-else", "boidws", ""} {
		body := createBody(map[string]interface{}{
			"Mounts": []map[string]interface{}{
				{"Type": "volume", "Source": name, "Target": "/data"},
			},
		})
		assertAllow(t, "POST", "/containers/create", body)
	}
}

// TestContainersCreate_volumesFrom_deny pins 経路 7. HostConfig.VolumesFrom
// inherits another container's ENTIRE mount set by container name, so no
// prefix rule can be applied to the request: the reserved name never appears
// in it. See hostConfig.VolumesFrom's doc comment for why the answer is a
// blanket deny rather than a smarter check.
func TestContainersCreate_volumesFrom_deny(t *testing.T) {
	body := createBody(map[string]interface{}{
		"VolumesFrom": []string{"some-container"},
	})
	assertDenyContains(t, "POST", "/containers/create", body, "VolumesFrom")

	// Case-variation, matching this file's existing parser-differential
	// posture (encoding/json matches field names case-insensitively, exactly
	// as the daemon's decoder does).
	assertDenyContains(t, "POST", "/containers/create",
		[]byte(`{"HostConfig":{"volumesfrom":["some-container:ro"]}}`), "VolumesFrom")
}

func TestContainersCreate_volumesFromEmpty_allow(t *testing.T) {
	assertAllow(t, "POST", "/containers/create", createBody(map[string]interface{}{
		"VolumesFrom": []string{},
	}))
}

// TestContainersCreate_bindsNamedVolume_deny pins the third shape of the same
// attack, which was ALREADY closed but had no test naming it: the legacy
// `Binds` string form takes a named volume on its left-hand side
// ("vol:/target") just as happily as a host path, and HostConfig.Binds is
// denied unconditionally. Pinned here so a future "relax Binds to allow named
// volumes" refactor has to confront 経路 6 explicitly.
func TestContainersCreate_bindsNamedVolume_deny(t *testing.T) {
	for _, bind := range []string{
		"boid-ws-home-abcd1234-default:/victim",
		"boid-ws-home-abcd1234-default:/victim:rw",
	} {
		assertDenyContains(t, "POST", "/containers/create",
			createBody(map[string]interface{}{"Binds": []string{bind}}), "Binds")
	}
}

// TestContainerStart_legacyHostConfigMount_deny pins the fourth shape: the
// pre-1.12 `POST /containers/{id}/start` body could carry a HostConfig, which
// would let a container created with a clean HostConfig acquire mounts at
// start time. checkContainerStart already refuses ANY non-empty HostConfig
// there (that is why this passes today); the test exists so the containment
// is anchored to this plan's threat model and not only to the older
// bind-mount one.
func TestContainerStart_legacyHostConfigMount_deny(t *testing.T) {
	body := mustJSON(map[string]interface{}{
		"HostConfig": map[string]interface{}{
			"Mounts": []map[string]interface{}{
				{"Type": "volume", "Source": "boid-ws-home-abcd1234-default", "Target": "/victim"},
			},
		},
	})
	assertDenyContains(t, "POST", "/containers/abc123/start", body, "HostConfig")

	assertDenyContains(t, "POST", "/containers/abc123/start",
		mustJSON(map[string]interface{}{
			"HostConfig": map[string]interface{}{"VolumesFrom": []string{"other"}},
		}), "HostConfig")
}
