package dockerproxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// helpers

func mustJSON(v interface{}) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func createBody(hc map[string]interface{}) []byte {
	return mustJSON(map[string]interface{}{"HostConfig": hc})
}

func assertAllow(t *testing.T, method, path string, body []byte) {
	t.Helper()
	v := CheckRequest(method, path, body)
	if !v.Allow {
		t.Errorf("expected ALLOW for %s %s, got DENY: %s", method, path, v.Reason)
	}
}

func assertDeny(t *testing.T, method, path string, body []byte) {
	t.Helper()
	v := CheckRequest(method, path, body)
	if v.Allow {
		t.Errorf("expected DENY for %s %s, got ALLOW", method, path)
	}
}

func assertDenyContains(t *testing.T, method, path string, body []byte, substr string) {
	t.Helper()
	v := CheckRequest(method, path, body)
	if v.Allow {
		t.Errorf("expected DENY for %s %s, got ALLOW", method, path)
		return
	}
	if !strings.Contains(v.Reason, substr) {
		t.Errorf("deny reason %q does not contain %q", v.Reason, substr)
	}
}

// --- stripVersion ---

func TestStripVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/v1.43/containers/create", "/containers/create"},
		{"/v1.43/", "/"},
		{"/v1.43", "/"},
		{"/containers/create", "/containers/create"},
		{"/", "/"},
	}
	for _, c := range cases {
		got := stripVersion(c.in)
		if got != c.want {
			t.Errorf("stripVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// --- GET/HEAD always allowed ---

func TestGetHeadAlwaysAllowed(t *testing.T) {
	for _, path := range []string{
		"/containers/json",
		"/version",
		"/_ping",
		"/info",
		"/images/json",
		"/v1.43/containers/json",
	} {
		assertAllow(t, "GET", path, nil)
		assertAllow(t, "HEAD", path, nil)
	}
}

// --- POST /containers/create: Binds (system 1) ---

func TestBindsPresent_deny(t *testing.T) {
	body := createBody(map[string]interface{}{
		"Binds": []string{"/host/path:/container/path"},
	})
	assertDenyContains(t, "POST", "/containers/create", body, "Binds")
}

func TestBindsEmpty_allow(t *testing.T) {
	body := createBody(map[string]interface{}{
		"Binds": []string{},
	})
	assertAllow(t, "POST", "/containers/create", body)
}

// --- POST /containers/create: Mounts type=bind (system 2) ---

func TestMountsTypeBind_deny(t *testing.T) {
	body := createBody(map[string]interface{}{
		"Mounts": []map[string]interface{}{
			{"Type": "bind", "Source": "/host/path", "Target": "/container/path"},
		},
	})
	assertDenyContains(t, "POST", "/containers/create", body, "type=bind")
}

// --- POST /containers/create: Mounts type=volume without DriverConfig (allow) ---

func TestMountsTypeVolume_noDriverConfig_allow(t *testing.T) {
	body := createBody(map[string]interface{}{
		"Mounts": []map[string]interface{}{
			{"Type": "volume", "Target": "/data"},
		},
	})
	assertAllow(t, "POST", "/containers/create", body)
}

// --- POST /containers/create: Mounts type=volume + local driver device= (system 3) ---

func TestMountsTypeVolume_localDriverDevice_deny(t *testing.T) {
	body := createBody(map[string]interface{}{
		"Mounts": []map[string]interface{}{
			{
				"Type":   "volume",
				"Target": "/container/path",
				"VolumeOptions": map[string]interface{}{
					"DriverConfig": map[string]interface{}{
						"Name":    "local",
						"Options": map[string]string{"type": "none", "device": "/etc", "o": "bind"},
					},
				},
			},
		},
	})
	assertDeny(t, "POST", "/containers/create", body)
}

func TestMountsTypeVolume_localDriverOBind_deny(t *testing.T) {
	body := createBody(map[string]interface{}{
		"Mounts": []map[string]interface{}{
			{
				"Type":   "volume",
				"Target": "/container/path",
				"VolumeOptions": map[string]interface{}{
					"DriverConfig": map[string]interface{}{
						"Name":    "local",
						"Options": map[string]string{"o": "bind,uid=1000"},
					},
				},
			},
		},
	})
	assertDeny(t, "POST", "/containers/create", body)
}

// --- Privileged ---

func TestPrivileged_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"Privileged": true})
	assertDenyContains(t, "POST", "/containers/create", body, "Privileged")
}

func TestPrivilegedFalse_allow(t *testing.T) {
	body := createBody(map[string]interface{}{"Privileged": false})
	assertAllow(t, "POST", "/containers/create", body)
}

// --- NetworkMode ---

func TestNetworkModeHost_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"NetworkMode": "host"})
	assertDenyContains(t, "POST", "/containers/create", body, "NetworkMode")
}

func TestNetworkModeContainerPrefix_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"NetworkMode": "container:abc123"})
	assertDenyContains(t, "POST", "/containers/create", body, "NetworkMode")
}

func TestNetworkModeNsPrefix_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"NetworkMode": "ns:/proc/1/ns/net"})
	assertDenyContains(t, "POST", "/containers/create", body, "NetworkMode")
}

func TestNetworkModeBridge_allow(t *testing.T) {
	body := createBody(map[string]interface{}{"NetworkMode": "bridge"})
	assertAllow(t, "POST", "/containers/create", body)
}

func TestNetworkModeNone_allow(t *testing.T) {
	body := createBody(map[string]interface{}{"NetworkMode": "none"})
	assertAllow(t, "POST", "/containers/create", body)
}

// TestPortBindings_deny pins §決定5's "host への port publish は非サポート"
// (docs/plans/phase6-container-backend.md §PR6): publishing a sibling
// container's port to the host would let it be reached directly by host
// IP, bypassing the workspace internal network entirely — the "直 IP 拒否"
// requirement.
func TestPortBindings_deny(t *testing.T) {
	body := createBody(map[string]interface{}{
		"PortBindings": map[string]interface{}{
			"80/tcp": []map[string]string{{"HostPort": "8080"}},
		},
	})
	assertDenyContains(t, "POST", "/containers/create", body, "PortBindings")
}

func TestPortBindings_empty_allow(t *testing.T) {
	body := createBody(map[string]interface{}{"PortBindings": map[string]interface{}{}})
	assertAllow(t, "POST", "/containers/create", body)
}

func TestPortBindings_absent_allow(t *testing.T) {
	body := createBody(map[string]interface{}{})
	assertAllow(t, "POST", "/containers/create", body)
}

// --- PidMode ---

func TestPidModeHost_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"PidMode": "host"})
	assertDenyContains(t, "POST", "/containers/create", body, "PidMode")
}

func TestPidModeContainer_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"PidMode": "container:abc"})
	assertDenyContains(t, "POST", "/containers/create", body, "PidMode")
}

// --- IpcMode ---

func TestIpcModeHost_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"IpcMode": "host"})
	assertDenyContains(t, "POST", "/containers/create", body, "IpcMode")
}

func TestIpcModeContainer_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"IpcMode": "container:abc"})
	assertDenyContains(t, "POST", "/containers/create", body, "IpcMode")
}

// --- UsernsMode / CgroupnsMode ---

func TestUsernsMode_host_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"UsernsMode": "host"})
	assertDenyContains(t, "POST", "/containers/create", body, "UsernsMode")
}

func TestCgroupnsMode_host_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"CgroupnsMode": "host"})
	assertDenyContains(t, "POST", "/containers/create", body, "CgroupnsMode")
}

// --- SecurityOpt ---

func TestSecurityOpt_seccompUnconfined_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"SecurityOpt": []string{"seccomp=unconfined"}})
	assertDenyContains(t, "POST", "/containers/create", body, "SecurityOpt")
}

func TestSecurityOpt_noNewPrivileges_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"SecurityOpt": []string{"no-new-privileges=false"}})
	assertDenyContains(t, "POST", "/containers/create", body, "SecurityOpt")
}

func TestSecurityOpt_empty_allow(t *testing.T) {
	body := createBody(map[string]interface{}{"SecurityOpt": []string{}})
	assertAllow(t, "POST", "/containers/create", body)
}

func TestSecurityOpt_absent_allow(t *testing.T) {
	body := createBody(map[string]interface{}{})
	assertAllow(t, "POST", "/containers/create", body)
}

// --- CapAdd ---

func TestCapAdd_netAdmin_deny(t *testing.T) {
	// Even non-obviously-dangerous capability names are denied (blanket policy).
	body := createBody(map[string]interface{}{"CapAdd": []string{"NET_ADMIN"}})
	assertDenyContains(t, "POST", "/containers/create", body, "CapAdd")
}

func TestCapAdd_empty_allow(t *testing.T) {
	body := createBody(map[string]interface{}{"CapAdd": []string{}})
	assertAllow(t, "POST", "/containers/create", body)
}

func TestCapAdd_absent_allow(t *testing.T) {
	body := createBody(map[string]interface{}{})
	assertAllow(t, "POST", "/containers/create", body)
}

// --- Devices ---

func TestDevices_deny(t *testing.T) {
	body := createBody(map[string]interface{}{
		"Devices": []map[string]string{{"PathOnHost": "/dev/sda", "PathInContainer": "/dev/sda"}},
	})
	assertDenyContains(t, "POST", "/containers/create", body, "Devices")
}

// --- Runtime ---

func TestRuntime_sysboxRunc_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"Runtime": "sysbox-runc"})
	assertDenyContains(t, "POST", "/containers/create", body, "Runtime")
}

func TestRuntime_runc_allow(t *testing.T) {
	body := createBody(map[string]interface{}{"Runtime": "runc"})
	assertAllow(t, "POST", "/containers/create", body)
}

func TestRuntime_empty_allow(t *testing.T) {
	body := createBody(map[string]interface{}{"Runtime": ""})
	assertAllow(t, "POST", "/containers/create", body)
}

// --- Sysctls ---

func TestSysctls_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"Sysctls": map[string]string{"net.ipv4.ip_forward": "1"}})
	assertDenyContains(t, "POST", "/containers/create", body, "Sysctls")
}

// --- CgroupParent ---

func TestCgroupParent_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"CgroupParent": "/custom/cgroup"})
	assertDenyContains(t, "POST", "/containers/create", body, "CgroupParent")
}

// --- DeviceCgroupRules ---

func TestDeviceCgroupRules_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"DeviceCgroupRules": []string{"c 1:3 mr"}})
	assertDenyContains(t, "POST", "/containers/create", body, "DeviceCgroupRules")
}

// --- POST /containers/{id}/exec ---

func TestExecCreate_privileged_deny(t *testing.T) {
	body := mustJSON(map[string]interface{}{"Privileged": true, "Cmd": []string{"sh"}})
	assertDenyContains(t, "POST", "/containers/abc123/exec", body, "Privileged")
}

func TestExecCreate_notPrivileged_allow(t *testing.T) {
	body := mustJSON(map[string]interface{}{"Privileged": false, "Cmd": []string{"sh"}})
	assertAllow(t, "POST", "/containers/abc123/exec", body)
}

func TestExecCreate_versionPrefix(t *testing.T) {
	body := mustJSON(map[string]interface{}{"Privileged": true})
	assertDeny(t, "POST", "/v1.43/containers/abc123/exec", body)
}

// --- POST /build and /session ---

func TestBuild_deny(t *testing.T) {
	assertDenyContains(t, "POST", "/build", nil, "image build")
}

func TestSession_deny(t *testing.T) {
	assertDenyContains(t, "POST", "/session", nil, "image build")
}

func TestBuild_versionPrefix_deny(t *testing.T) {
	assertDeny(t, "POST", "/v1.43/build", nil)
}

// --- POST /containers/{id}/start ---

func TestContainerStart_noBody_allow(t *testing.T) {
	assertAllow(t, "POST", "/containers/abc123/start", nil)
}

func TestContainerStart_emptyBody_allow(t *testing.T) {
	assertAllow(t, "POST", "/containers/abc123/start", []byte("{}"))
}

func TestContainerStart_withHostConfig_deny(t *testing.T) {
	body := mustJSON(map[string]interface{}{
		"HostConfig": map[string]interface{}{"Binds": []string{"/foo:/bar"}},
	})
	assertDenyContains(t, "POST", "/containers/abc123/start", body, "HostConfig")
}

func TestContainerStart_withNullHostConfig_allow(t *testing.T) {
	body := mustJSON(map[string]interface{}{"HostConfig": nil})
	assertAllow(t, "POST", "/containers/abc123/start", body)
}

// --- Unknown mutating endpoint (fail-closed) ---

func TestUnknownMutating_deny(t *testing.T) {
	assertDeny(t, "POST", "/some/new/api", nil)
	assertDeny(t, "PUT", "/containers/abc/config", nil)
}

// --- MaxBodyBytes exceeded ---

func TestMaxBodyBytes_exceeded_deny(t *testing.T) {
	// Build a body larger than MaxBodyBytes by padding a valid JSON object.
	// Use a large string value to exceed the limit without valid JSON parse issues.
	big := bytes.Repeat([]byte("x"), MaxBodyBytes+1)
	body := append([]byte(`{"HostConfig":{"Runtime":"`), big...)
	body = append(body, []byte(`"}}`)...)
	assertDeny(t, "POST", "/containers/create", body)
}

// --- Parser differential: duplicate HostConfig key ---
// Go's encoding/json uses the LAST value for duplicate keys (same as Docker daemon).
// A duplicated key attack tries to sneak a dangerous value past a checker that
// only sees the first occurrence. Our decoder uses encoding/json identically,
// so the last "Privileged" wins — the dangerous one.
func TestParserDifferential_duplicateKey_deny(t *testing.T) {
	// Craft raw JSON with duplicate Privileged keys: first false, last true.
	raw := []byte(`{"HostConfig":{"Privileged":false,"Privileged":true}}`)
	v := CheckRequest("POST", "/containers/create", raw)
	if v.Allow {
		t.Error("expected DENY for duplicate key attack (last Privileged=true should win)")
	}
}

// --- Parser differential: case-variation attack ---
// encoding/json matches struct field names case-insensitively, same as Docker daemon,
// so "privileged" / "PRIVILEGED" all map to Privileged bool.
func TestParserDifferential_caseVariation_deny(t *testing.T) {
	raw := []byte(`{"HostConfig":{"privileged":true}}`)
	assertDeny(t, "POST", "/containers/create", raw)
}

func TestParserDifferential_caseVariationCapAdd_deny(t *testing.T) {
	raw := []byte(`{"HostConfig":{"capadd":["NET_ADMIN"]}}`)
	assertDeny(t, "POST", "/containers/create", raw)
}

func TestParserDifferential_caseVariationBinds_deny(t *testing.T) {
	raw := []byte(`{"HostConfig":{"binds":["/etc:/etc"]}}`)
	assertDeny(t, "POST", "/containers/create", raw)
}

// --- Empty body and nil body ---

func TestContainersCreate_emptyBody_allow(t *testing.T) {
	assertAllow(t, "POST", "/containers/create", []byte{})
}

func TestContainersCreate_nilBody_allow(t *testing.T) {
	assertAllow(t, "POST", "/containers/create", nil)
}

// --- Version prefix routing ---

func TestVersionPrefix_containersCreate_deny(t *testing.T) {
	body := createBody(map[string]interface{}{"Privileged": true})
	assertDeny(t, "POST", "/v1.43/containers/create", body)
}

func TestVersionPrefix_containersCreate_allow(t *testing.T) {
	body := createBody(map[string]interface{}{})
	assertAllow(t, "POST", "/v1.43/containers/create", body)
}

// --- DELETE endpoints are allowed ---

func TestDeleteContainer_allow(t *testing.T) {
	assertAllow(t, "DELETE", "/containers/abc123", nil)
}

func TestDeleteImage_allow(t *testing.T) {
	assertAllow(t, "DELETE", "/images/myimage", nil)
}

// --- Explicit allowed POST endpoints pass through ---

func TestContainerStop_allow(t *testing.T) {
	assertAllow(t, "POST", "/containers/abc123/stop", nil)
}

func TestContainerWait_allow(t *testing.T) {
	assertAllow(t, "POST", "/containers/abc123/wait", nil)
}

func TestExecStart_allow(t *testing.T) {
	assertAllow(t, "POST", "/exec/abc123/start", nil)
}

func TestImageCreate_allow(t *testing.T) {
	// pull is allowed
	assertAllow(t, "POST", "/images/create", nil)
}
