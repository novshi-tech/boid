package dockerproxy

import (
	"strings"
	"testing"
)

// This file covers the exposure docs/plans/workspace-home-volume-persistence.md
// recorded under "未解決 / 本 doc の範囲外": PR1 reserved the volume namespace
// boid OWNS BY NAME ("boid-ws-", a static prefix), which protects the
// per-workspace HOME volumes — but the daemon's own state volume is named by
// the COMPOSE PROJECT, not by boid ("boid_boid_state" under
// build/container/compose.yml's `name: boid`), so it matched no prefix and a
// `capabilities.docker` job could mount it into a sibling container and read
// secret.key / boid.db / tls/ca.key / web_secret / install_id wholesale.
//
// The fix keeps PR1's three name-dependent policy paths exactly as they were
// and only widens the SET they consult: static prefix ∪ names discovered at
// daemon startup (internal/dispatcher.DetectDaemonStateVolumes). These tests
// pin all three paths against the widened set, and pin that the static prefix
// still fires on its own so the runtime set can never be mistaken for a
// replacement of it.

// daemonStateVolume is the real name the live rootless-podman dogfood deploy
// produces: compose's `name: boid` + the `boid_state` volume key. Using the
// real string (not a placeholder) keeps this test honest about the shape the
// rule has to match — an underscore-joined name that shares no prefix with
// anything boid generates itself.
const daemonStateVolume = "boid_boid_state"

func reservedForTest() ReservedVolumes {
	return NewReservedVolumes([]string{daemonStateVolume})
}

func assertDenyReserved(t *testing.T, method, path string, body []byte, substr string) {
	t.Helper()
	v := CheckRequest(method, path, body, true, reservedForTest())
	if v.Allow {
		t.Errorf("expected DENY for %s %s, got ALLOW", method, path)
		return
	}
	if !strings.Contains(v.Reason, substr) {
		t.Errorf("deny reason %q does not contain %q", v.Reason, substr)
	}
}

func assertAllowReserved(t *testing.T, method, path string, body []byte) {
	t.Helper()
	v := CheckRequest(method, path, body, true, reservedForTest())
	if !v.Allow {
		t.Errorf("expected ALLOW for %s %s, got DENY: %s", method, path, v.Reason)
	}
}

// --- path 1: POST /volumes/create (idempotent — "creating" the daemon's own
// volume would enrol the REAL one in this job's ledger and have Reap destroy
// it when the job ends). ---

func TestVolumesCreate_daemonStateVolume_deny(t *testing.T) {
	assertDenyReserved(t, "POST", "/volumes/create",
		mustJSON(map[string]interface{}{"Name": daemonStateVolume}), "reserved by boid")
	// The reason must say WHICH rule fired: "another workspace's HOME" and
	// "the daemon's own secrets" are very different reports to an operator.
	assertDenyReserved(t, "POST", "/v1.43/volumes/create",
		mustJSON(map[string]interface{}{"Name": daemonStateVolume}), "own state volume")
}

// --- path 2: DELETE /volumes/{name} ---

func TestVolumesDelete_daemonStateVolume_deny(t *testing.T) {
	assertDenyReserved(t, "DELETE", "/volumes/"+daemonStateVolume, nil, "reserved by boid")
	assertDenyReserved(t, "DELETE", "/v1.43/volumes/"+daemonStateVolume, nil, "reserved by boid")
}

// --- path 3: POST /containers/create HostConfig.Mounts[].Source — the one
// that actually leaks the secrets, and the only one that needs no write
// access at all. ---

func TestContainersCreate_mountDaemonStateVolume_deny(t *testing.T) {
	body := createBody(map[string]interface{}{
		"Mounts": []map[string]interface{}{
			{"Type": "volume", "Source": daemonStateVolume, "Target": "/state"},
		},
	})
	assertDenyReserved(t, "POST", "/containers/create", body, "reserved by boid")
}

// TestContainersCreate_mountDaemonStateVolume_typeIndependent mirrors PR1's
// mountSpec.Source contract: the Source check must not depend on modelling
// the engine's Type normalization, so an omitted / unusual Type must not
// become a bypass.
func TestContainersCreate_mountDaemonStateVolume_typeIndependent(t *testing.T) {
	for _, typ := range []string{"", "VOLUME", "Volume", "unknown-future-type"} {
		body := createBody(map[string]interface{}{
			"Mounts": []map[string]interface{}{
				{"Type": typ, "Source": daemonStateVolume, "Target": "/state"},
			},
		})
		assertDenyReserved(t, "POST", "/containers/create", body, "reserved by boid")
	}
}

// --- the blast radius: names NOT in the runtime set stay allowed ---

func TestDaemonStateVolume_unrelatedNames_stillAllowed(t *testing.T) {
	for _, name := range []string{
		"my-volume",
		"boid_myapp",     // the false positive a blanket "boid_" prefix deny would cause
		"boid_state",     // the compose KEY, not the engine-visible name
		"boid_boid_stat", // near-miss: exact match only, no prefix semantics
		"boid_boid_state_backup",
	} {
		assertAllowReserved(t, "POST", "/volumes/create", mustJSON(map[string]interface{}{"Name": name}))
		assertAllowReserved(t, "DELETE", "/volumes/"+name, nil)
		assertAllowReserved(t, "POST", "/containers/create", createBody(map[string]interface{}{
			"Mounts": []map[string]interface{}{{"Type": "volume", "Source": name, "Target": "/x"}},
		}))
	}
}

// TestDaemonStateVolume_staticPrefixStillApplies pins that the runtime set is
// ADDITIVE. A regression that replaced dockerres.IsReservedVolumeName with the
// runtime set would leave every test above green while re-opening all of PR1's
// 経路 4/6 on the workspace HOME volumes.
func TestDaemonStateVolume_staticPrefixStillApplies(t *testing.T) {
	assertDenyReserved(t, "POST", "/volumes/create",
		mustJSON(map[string]interface{}{"Name": "boid-ws-home-abcd1234-default"}), "reserved by boid")
	assertDenyReserved(t, "DELETE", "/volumes/boid-ws-home-abcd1234-default", nil, "reserved by boid")
	assertDenyReserved(t, "POST", "/containers/create", createBody(map[string]interface{}{
		"Mounts": []map[string]interface{}{
			{"Type": "volume", "Source": "boid-ws-abcd1234-default", "Target": "/x"},
		},
	}), "reserved by boid")
}

// TestZeroReservedVolumes_behavesExactlyLikePR1 pins the zero value: a Server
// that never learned a daemon state volume name (bare `boid start`, or a
// detection failure) must keep PR1's behavior byte-for-byte rather than
// failing open OR failing closed.
func TestZeroReservedVolumes_behavesExactlyLikePR1(t *testing.T) {
	var zero ReservedVolumes
	if v := CheckRequest("POST", "/volumes/create",
		mustJSON(map[string]interface{}{"Name": daemonStateVolume}), true, zero); !v.Allow {
		t.Errorf("zero ReservedVolumes denied %q; the daemon state rule must be inert when nothing was detected", daemonStateVolume)
	}
	if v := CheckRequest("POST", "/volumes/create",
		mustJSON(map[string]interface{}{"Name": "boid-ws-home-x"}), true, zero); v.Allow {
		t.Error("zero ReservedVolumes allowed a boid-ws- name; the static prefix must not depend on runtime detection")
	}
}

// TestVolumesFrom_stillDeniedOutright is the "confirm only" item: PR1 already
// denies HostConfig.VolumesFrom unconditionally, which is what covers the
// shape this PR's name-based rule structurally cannot (the request names a
// container, not a volume — so "does this inherit the daemon state volume?"
// is unanswerable from the body). No new code; this pins that the existing
// guard is still the thing standing between a job and the daemon container's
// entire mount set.
func TestVolumesFrom_stillDeniedOutright(t *testing.T) {
	body := createBody(map[string]interface{}{"VolumesFrom": []string{"some-container"}})
	assertDenyReserved(t, "POST", "/containers/create", body, "VolumesFrom")
}
