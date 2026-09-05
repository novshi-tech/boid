//go:build !linux

package cmd

import (
	"context"
	"fmt"
	"runtime"

	"github.com/novshi-tech/boid/internal/client"
)

// This file is the non-Linux counterpart of host.go's entry points: boid's
// daemon is Linux-only, so on every other GOOS host mode is disabled and
// the CLI is unconditionally a remote client.

// hostModeEnabled always reports false off Linux.
func hostModeEnabled() bool { return false }

// resolveHostModeClient is unreachable off Linux; it exists to satisfy the
// compiler and returns an explanatory error if ever called.
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
