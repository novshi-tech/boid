package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/testutil"
)

func TestWebCommandRegistration(t *testing.T) {
	subcommands := map[string]bool{}
	for _, c := range webCmd.Commands() {
		subcommands[c.Name()] = true
	}
	for _, name := range []string{"pair", "devices", "revoke", "revoke-all", "set-url"} {
		if !subcommands[name] {
			t.Errorf("webCmd missing subcommand %q", name)
		}
	}
}

func TestWebCmdRegisteredOnRoot(t *testing.T) {
	found := false
	for _, c := range rootCmd.Commands() {
		if c.Name() == "web" {
			found = true
			break
		}
	}
	if !found {
		t.Error("rootCmd does not have 'web' subcommand")
	}
}

func TestWebRevokeRequiresArg(t *testing.T) {
	if err := webRevokeCmd.Args(webRevokeCmd, []string{}); err == nil {
		t.Error("revoke: expected error for missing id arg")
	}
	if err := webRevokeCmd.Args(webRevokeCmd, []string{"abc"}); err != nil {
		t.Errorf("revoke: unexpected error for single arg: %v", err)
	}
}

func TestWebSetURLRequiresArg(t *testing.T) {
	if err := webSetURLCmd.Args(webSetURLCmd, []string{}); err == nil {
		t.Error("set-url: expected error for missing URL arg")
	}
	if err := webSetURLCmd.Args(webSetURLCmd, []string{"https://example.com"}); err != nil {
		t.Errorf("set-url: unexpected error for single arg: %v", err)
	}
}

// TestWebSetURLIsRemoteScope pins docs/plans/release-onboarding.md 穴8
// (b): `web set-url`/`set-addr` now go through the daemon's
// POST /api/config/mutate, same as `boid config set` — no longer a
// scopeLocal command editing a local config.yaml this host cannot share
// with a compose daemon.
func TestWebSetURLIsRemoteScope(t *testing.T) {
	if webSetURLCmd.Annotations[scopeAnnotationKey] != scopeRemote {
		t.Errorf("set-url: want scopeRemote, got %q", webSetURLCmd.Annotations[scopeAnnotationKey])
	}
	if webSetAddrCmd.Annotations[scopeAnnotationKey] != scopeRemote {
		t.Errorf("set-addr: want scopeRemote, got %q", webSetAddrCmd.Annotations[scopeAnnotationKey])
	}
}

func TestRunWebSetURL_AppliesViaConfigMutate(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	var out bytes.Buffer
	webSetURLCmd.SetOut(&out)

	if err := runWebSetURL(webSetURLCmd, []string{"https://boid.example.com"}); err != nil {
		t.Fatalf("runWebSetURL: %v", err)
	}
	if !strings.Contains(out.String(), "web.public_url = https://boid.example.com") {
		t.Errorf("unexpected output: %q", out.String())
	}

	var getOut bytes.Buffer
	getCmd := configGetCmd
	getCmd.SetOut(&getOut)
	if err := runConfigGet(getCmd, []string{"web.public_url"}); err != nil {
		t.Fatalf("runConfigGet: %v", err)
	}
	if got := strings.TrimSpace(getOut.String()); got != "https://boid.example.com" {
		t.Errorf("get web.public_url = %q, want https://boid.example.com", got)
	}
}

func TestRunWebSetAddr_AppliesViaConfigMutate(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	var out bytes.Buffer
	webSetAddrCmd.SetOut(&out)

	if err := runWebSetAddr(webSetAddrCmd, []string{"0.0.0.0:8080"}); err != nil {
		t.Fatalf("runWebSetAddr: %v", err)
	}
	if !strings.Contains(out.String(), "web.http_addr = 0.0.0.0:8080") {
		t.Errorf("unexpected output: %q", out.String())
	}

	var getOut bytes.Buffer
	getCmd := configGetCmd
	getCmd.SetOut(&getOut)
	if err := runConfigGet(getCmd, []string{"web.http_addr"}); err != nil {
		t.Fatalf("runConfigGet: %v", err)
	}
	if got := strings.TrimSpace(getOut.String()); got != "0.0.0.0:8080" {
		t.Errorf("get web.http_addr = %q, want 0.0.0.0:8080", got)
	}
}
