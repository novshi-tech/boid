package cmd

import (
	"fmt"
	"io"

	"github.com/novshi-tech/boid/internal/client"
)

// proxyPortFetcher fetches the daemon's egress proxy port. Package-level
// variable so unit tests can substitute a fake without touching the daemon
// socket. The default queries the running daemon via /api/proxy.
var proxyPortFetcher = func() (int, error) {
	c := client.NewUnixClient(client.DefaultSocketPath())
	var proxyInfo struct{ Port int }
	if err := c.Do("GET", "/api/proxy", nil, &proxyInfo); err != nil {
		return 0, err
	}
	return proxyInfo.Port, nil
}

// resolveDaemonProxyPort returns the egress proxy port managed by the boid
// daemon, or 0 when the daemon is not reachable. ProfileInit sandboxes
// (used by `boid kit init` and `boid workspace configure`) inherit the
// daemon's network namespace policy: pasta gives them a fresh netns where
// nftables only permits CONNECT to the proxy port. Without HTTPS_PROXY
// pointing at that port, in-sandbox HTTPS clients (claude / codex / opencode
// harnesses, curl, npm registry, ...) cannot open any outbound TCP socket
// and surface as `FailedToOpenSocket` / `Connection refused`.
//
// When the daemon is not running, the returned port is 0 — kit init is
// designed to work pre-onboarding when no daemon exists. A warning is
// printed so users running an AI-harness pre-`boid start` understand why
// network calls will fail.
func resolveDaemonProxyPort(out io.Writer) int {
	port, err := proxyPortFetcher()
	if err != nil {
		// Daemon is most likely not running (or stale socket); fall back to
		// no-proxy mode. The warning calls out the AI-harness consequence
		// so the user does not chase ghost network bugs.
		fmt.Fprintln(out, "warning: boid daemon is not running, so the sandbox will start without an egress proxy.")
		fmt.Fprintln(out, "  AI agent harnesses (claude / codex / opencode) need the proxy to reach api.anthropic.com etc;")
		fmt.Fprintln(out, "  start the daemon in another shell with `boid start` and re-run this command,")
		fmt.Fprintln(out, "  or use the shell harness if no network egress is needed.")
		return 0
	}
	return port
}
