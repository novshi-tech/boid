package upstream

import (
	"net"
	"strings"
	"testing"
)

func mustListen(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	return ln
}

// These are pure-logic / error-path tests that never exec a real git
// binary against a real repository, so (unlike upstream_e2e_test.go) they
// run under plain `go test ./...` regardless of host git availability. The
// full serve/clone/push round trip is covered by the //go:build e2e file.

func TestRepoDirName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"app", "app.git"},
		{"app.git", "app.git"},
		{"", ".git"},
	}
	for _, tt := range tests {
		if got := repoDirName(tt.name); got != tt.want {
			t.Errorf("repoDirName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestFindGitHTTPBackend_FallsBackToExecPathQuery(t *testing.T) {
	// Force the well-known-paths fast path to miss so this exercises the
	// `git --exec-path` fallback, without depending on whether the test
	// host's real git-core happens to live in one of those directories.
	orig := wellKnownGitExecDirs
	wellKnownGitExecDirs = []string{t.TempDir()}
	t.Cleanup(func() { wellKnownGitExecDirs = orig })

	if _, _, err := findGitHTTPBackend("/no/such/git-binary-anywhere"); err == nil {
		t.Fatal("expected error when no well-known dir matches and git-bin is invalid")
	}
}

func TestNew_NoGitHTTPBackendAnywhere(t *testing.T) {
	orig := wellKnownGitExecDirs
	wellKnownGitExecDirs = []string{t.TempDir()}
	t.Cleanup(func() { wellKnownGitExecDirs = orig })

	_, err := New(Options{GitBin: "/no/such/git-binary-anywhere"})
	if err == nil {
		t.Fatal("expected error when git-http-backend cannot be located")
	}
	if !strings.Contains(err.Error(), "git-http-backend") {
		t.Errorf("error = %v, want it to mention git-http-backend", err)
	}
}

func TestInitBareRepo_InvalidGitBin(t *testing.T) {
	dir := t.TempDir()
	if _, err := InitBareRepo("/no/such/git-binary-anywhere", dir, "app"); err == nil {
		t.Fatal("expected error for a nonexistent git binary, got nil")
	}
}

func TestURL_AppendsDotGitOnce(t *testing.T) {
	u := &Upstream{ln: mustListen(t)}
	defer u.ln.Close()

	got := u.URL("app")
	if !strings.HasSuffix(got, "/app.git") {
		t.Errorf("URL(%q) = %q, want suffix /app.git", "app", got)
	}
	got2 := u.URL("app.git")
	if got2 != got {
		t.Errorf("URL(%q) = %q, want %q (idempotent .git suffix)", "app.git", got2, got)
	}
}
