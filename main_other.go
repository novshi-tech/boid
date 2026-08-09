//go:build !linux

package main

import (
	"fmt"
	"os"

	"github.com/novshi-tech/boid/cmd"
)

// main is the entrypoint for every non-Linux build — the portable CLI
// client (docs/plans/windows-client-build.md).
//
// It is deliberately just cmd.Execute(). main.go's version carries two
// extra entrypoints that only exist inside a sandbox on the daemon's own
// host, and neither can be reached here:
//
//   - the builtin shim (BOID_BUILTIN_SHIM=1, sandbox.RunBoidShim), which
//     re-routes a nested `boid <subcommand>` call made by a hook or agent
//     back to the broker over a socket that only exists inside a running
//     job container; and
//   - the host-command shim (argv0 != "boid", sandbox.ShimExec), which is
//     the same binary bind-mounted into a sandbox under another name.
//
// Both are dispatch-time machinery of a Linux-only daemon. A Windows or
// macOS boid is a remote CLI: it never runs inside a sandbox, so it is
// never argv0'd as anything but "boid" and never sees BOID_BUILTIN_SHIM.
func main() {
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
