package gitgateway

import "testing"

// TestSandboxURL_UsernsUnchanged pins docs/plans/phase6-container-backend.md
// §PR4's "既存の URL 生成の無条件切り替え禁止" constraint: BackendUserns (and
// the zero value, so pre-existing callers keep compiling and behaving the
// same) must reproduce today's loopback-projection URL byte-for-byte.
func TestSandboxURL_UsernsUnchanged(t *testing.T) {
	got := SandboxURL(SandboxURLOptions{Backend: BackendUserns, Port: 54321})
	want := "http://10.0.2.2:54321"
	if got != want {
		t.Errorf("SandboxURL(userns) = %q, want %q", got, want)
	}
}

func TestSandboxURL_ZeroValueBackendMatchesUserns(t *testing.T) {
	got := SandboxURL(SandboxURLOptions{Port: 54321})
	want := SandboxURL(SandboxURLOptions{Backend: BackendUserns, Port: 54321})
	if got != want {
		t.Errorf("zero-value Backend = %q, want it to match BackendUserns = %q", got, want)
	}
}

func TestSandboxURL_ContainerUsesServiceNameOverTLS(t *testing.T) {
	got := SandboxURL(SandboxURLOptions{Backend: BackendContainer, Port: 443, ServiceName: "boid-daemon"})
	want := "https://boid-daemon:443"
	if got != want {
		t.Errorf("SandboxURL(container) = %q, want %q", got, want)
	}
}

func TestSandboxURL_ContainerDefaultsServiceName(t *testing.T) {
	got := SandboxURL(SandboxURLOptions{Backend: BackendContainer, Port: 443})
	want := "https://boid-gateway:443"
	if got != want {
		t.Errorf("SandboxURL(container, no ServiceName) = %q, want %q", got, want)
	}
}
