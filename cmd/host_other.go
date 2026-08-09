//go:build !linux

package cmd

import (
	"context"
	"fmt"
	"runtime"

	"github.com/novshi-tech/boid/internal/client"
)

// This file is the non-Linux counterpart of host.go's entry points.
//
// Host mode brings up and talks to a LOCAL containerized daemon: it runs
// scripts/deploy-container.sh, waits on a docker/podman compose stack, and
// connects to 127.0.0.1:8442. None of that can exist on a platform boid's
// daemon does not run on (CLAUDE.md: Linux only), so on every other GOOS
// the CLI is unconditionally a REMOTE client — PersistentPreRunE falls
// through to the ordinary profiles.Resolve path, which is what
// `boid login <url>` populates.

// hostModeEnabled always reports false off Linux, so cmd/root.go's
// PersistentPreRunE takes the profiles.Resolve branch for every command.
//
// This is the whole mechanism by which a Windows/macOS boid becomes a
// remote-only CLI: no new branch in root.go, just the local-daemon
// shortcut reporting that it has nothing to offer.
func hostModeEnabled() bool { return false }

// resolveHostModeClient is unreachable: root.go only calls it when
// hostModeEnabled() reports true, and the stub above never does. It exists
// to satisfy the compiler, and returns an explanatory error rather than
// panicking in case that guard is ever restructured.
func resolveHostModeClient(context.Context) (*client.Client, error) {
	return nil, errHostModeUnsupported()
}

// resolveHostModeClientNoAutostart is unreachable for the same reason as
// resolveHostModeClient above.
func resolveHostModeClientNoAutostart(context.Context) (*client.Client, error) {
	return nil, errHostModeUnsupported()
}

func errHostModeUnsupported() error {
	return fmt.Errorf("host mode (a local containerized daemon) is not available on %s: boid's daemon is linux-only; connect to a remote daemon with 'boid login <url>'", runtime.GOOS)
}
