package cmd

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestResolveDaemonProxyPort_DaemonReachable(t *testing.T) {
	orig := proxyPortFetcher
	defer func() { proxyPortFetcher = orig }()
	proxyPortFetcher = func() (int, error) { return 41659, nil }

	var out bytes.Buffer
	got := resolveDaemonProxyPort(&out)
	if got != 41659 {
		t.Fatalf("got %d, want 41659", got)
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected output on success: %q", out.String())
	}
}

func TestResolveDaemonProxyPort_DaemonNotRunning(t *testing.T) {
	orig := proxyPortFetcher
	defer func() { proxyPortFetcher = orig }()
	proxyPortFetcher = func() (int, error) { return 0, errors.New("dial unix: connect: connection refused") }

	var out bytes.Buffer
	got := resolveDaemonProxyPort(&out)
	if got != 0 {
		t.Fatalf("got %d, want 0 (fallback)", got)
	}
	s := out.String()
	for _, needle := range []string{
		"warning",
		"daemon is not running",
		"AI agent harnesses",
		"boid start",
	} {
		if !strings.Contains(s, needle) {
			t.Fatalf("warning missing %q: %q", needle, s)
		}
	}
}
