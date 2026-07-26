package dispatcher

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWire_DaemonStateVolume_ReachesDockerProxyPolicy is the seam test for
// this PR: it runs the WHOLE chain in one test, from a real /proc/self/mountinfo
// fixture to an actual HTTP 403 on an actual proxy socket.
//
//	/proc/self/mountinfo (podman fixture)
//	  -> DetectDaemonStateVolumes  (self-inspect at daemon startup)
//	  -> WireConfig.ReservedVolumeNames -> Runner.ReservedVolumeNames
//	  -> startDockerProxy -> Server.SetReservedVolumeNames
//	  -> CheckRequest -> 403 on POST /containers/create
//
// It exists because this repo keeps producing the same defect class: both
// ends of a wiring seam are correct and tested, and nothing crosses. The
// dockerproxy-side unit tests (policy_daemon_state_volume_test.go) would stay
// green if startDockerProxy never called SetReservedVolumeNames, and the
// detection tests (daemon_state_volume_test.go) would stay green if Wire
// dropped the field — in both cases the daemon state volume would be mountable
// from inside a sandbox exactly as it is today. Same construction as PR2's
// TestWire_DataHomeDir_ReachesMarkerAndLockOnDisk and PR3's
// TestDispatch_EmbeddedSkills_ReachLaunchedSpecAsPerSkillReadOnlyBinds.
//
// The complementary half — internal/server's buildRuntime actually feeding
// the detected names into WireConfig — is pinned by
// TestServer_New_RunnerReservedVolumeNamesComeFromSelfInspect
// (internal/server/wire_daemon_state_volume_test.go).
func TestWire_DaemonStateVolume_ReachesDockerProxyPolicy(t *testing.T) {
	dir := shortTempDir(t)

	// 1. Daemon startup: identify our own container and its named volumes.
	installProcFixtures(t, podmanMountInfo, cgroupV2Unhelpful)
	api := &stubInspector{mounts: composeDaemonMounts()}
	detected, err := DetectDaemonStateVolumes(context.Background(), api, "/home/boid/.local/share/boid")
	if err != nil {
		t.Fatalf("DetectDaemonStateVolumes: %v", err)
	}
	if len(detected.Names) == 0 {
		t.Fatal("test setup bug: nothing detected, so the rest of the chain proves nothing")
	}

	// 2. Wire must carry the names onto the Runner.
	r := Wire(WireConfig{ReservedVolumeNames: detected.Names, RuntimesDir: dir})
	if len(r.ReservedVolumeNames) != len(detected.Names) {
		t.Fatalf("Wire dropped ReservedVolumeNames: got %v, want %v", r.ReservedVolumeNames, detected.Names)
	}

	// 3. A per-job proxy, exactly as Dispatch starts one. The fake upstream
	//    answers 204 to anything that gets past the policy, so an ALLOW is
	//    observably different from a DENY.
	upstreamSock := filepath.Join(dir, "docker.sock")
	startFakeDockerUpstream(t, upstreamSock)
	t.Setenv("DOCKER_HOST", "unix://"+upstreamSock)

	ds, err := r.startDockerProxy("wire-daemon-state-volume")
	if err != nil {
		t.Fatalf("startDockerProxy: %v", err)
	}
	t.Cleanup(func() { _ = ds.proxy.Close() })

	// 4. The actual attack, over the actual socket a sandboxed docker client
	//    is handed: mount the daemon's own state volume into a sibling.
	status, body := postOverProxy(t, ds.socketPath, "/v1.43/containers/create", map[string]interface{}{
		"Image": "busybox",
		"Cmd":   []string{"sh", "-c", "cat /state/.local/share/boid/secret.key"},
		"HostConfig": map[string]interface{}{
			"Mounts": []map[string]interface{}{
				{"Type": "volume", "Source": "boid_boid_state", "Target": "/state"},
			},
		},
	})
	if status != http.StatusForbidden {
		t.Fatalf("mounting the daemon state volume returned %d (%s), want 403", status, strings.TrimSpace(body))
	}
	if !strings.Contains(body, "boid_boid_state") {
		t.Errorf("403 body %q does not name the offending volume", strings.TrimSpace(body))
	}

	// 5. Control: an unrelated volume must still be forwarded upstream, or
	//    this "fix" is just a broken proxy.
	status, body = postOverProxy(t, ds.socketPath, "/v1.43/containers/create", map[string]interface{}{
		"Image": "busybox",
		"HostConfig": map[string]interface{}{
			"Mounts": []map[string]interface{}{
				{"Type": "volume", "Source": "job-scratch", "Target": "/scratch"},
			},
		},
	})
	if status == http.StatusForbidden {
		t.Errorf("an ordinary named volume was denied too: %s", strings.TrimSpace(body))
	}
}

// TestWire_NoDaemonStateVolume_LeavesProxyUnchanged is the bare-`boid start`
// side of the same seam: nothing detected must mean nothing new denied, so
// the fix cannot regress a host-process deployment or a docker-using hook.
func TestWire_NoDaemonStateVolume_LeavesProxyUnchanged(t *testing.T) {
	dir := shortTempDir(t)

	// A data root that exists and resolves: DetectDaemonStateVolumes compares
	// the RESOLVED path against the mount table (resolveDataHomeForMountComparison),
	// and an unresolvable one is an undecidable that fails toward warning —
	// which is the opposite of the bare-host case this test is about.
	dataHome := filepath.Join(dir, "data")
	if err := os.MkdirAll(dataHome, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	installProcFixtures(t, mountInfoFixture(""), cgroupV2Unhelpful)
	api := &stubInspector{}
	detected, err := DetectDaemonStateVolumes(context.Background(), api, dataHome)
	if err != nil {
		t.Fatalf("DetectDaemonStateVolumes: %v", err)
	}
	if detected.StateVolumeExpected || len(detected.Names) != 0 {
		t.Fatalf("test setup bug: detected %+v on a plain host", detected)
	}

	r := Wire(WireConfig{ReservedVolumeNames: detected.Names, RuntimesDir: dir})

	upstreamSock := filepath.Join(dir, "docker.sock")
	startFakeDockerUpstream(t, upstreamSock)
	t.Setenv("DOCKER_HOST", "unix://"+upstreamSock)

	ds, err := r.startDockerProxy("wire-no-daemon-state-volume")
	if err != nil {
		t.Fatalf("startDockerProxy: %v", err)
	}
	t.Cleanup(func() { _ = ds.proxy.Close() })

	status, body := postOverProxy(t, ds.socketPath, "/v1.43/containers/create", map[string]interface{}{
		"HostConfig": map[string]interface{}{
			"Mounts": []map[string]interface{}{
				{"Type": "volume", "Source": "boid_boid_state", "Target": "/state"},
			},
		},
	})
	if status == http.StatusForbidden {
		t.Errorf("nothing was detected, yet the proxy denied %q: %s", "boid_boid_state", strings.TrimSpace(body))
	}

	// The static PR1 namespace is not runtime-detected and must still fire.
	status, _ = postOverProxy(t, ds.socketPath, "/v1.43/containers/create", map[string]interface{}{
		"HostConfig": map[string]interface{}{
			"Mounts": []map[string]interface{}{
				{"Type": "volume", "Source": "boid-ws-home-abcd1234-default", "Target": "/x"},
			},
		},
	})
	if status != http.StatusForbidden {
		t.Errorf("workspace HOME volume mount returned %d, want 403 (PR1's static namespace must not depend on detection)", status)
	}
}

// shortTempDir is t.TempDir() with a short basename. The per-job docker proxy
// socket lives under RuntimesDir when ServerSocket is unset, and t.TempDir()
// embeds the (long) test name, which pushes the socket path past the 108-byte
// AF_UNIX limit — the very failure mode dockerProxySocketPath's own doc
// comment describes.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "boid-dsv")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// postOverProxy issues a real POST through the per-job docker proxy socket and
// returns the status code and body.
func postOverProxy(t *testing.T, socketPath, path string, payload map[string]interface{}) (int, string) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	cli := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socketPath)
		},
	}}
	req, err := http.NewRequest(http.MethodPost, "http://docker"+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cli.Do(req)
	if err != nil {
		t.Fatalf("POST %s over proxy: %v", path, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, string(respBody)
}
