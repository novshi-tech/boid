//go:build !linux

package client

import (
	"fmt"
	"os"
	"runtime"
)

// lockAutostart fails closed on every non-Linux build: reaching it means
// the CLI was about to spawn a local daemon, and boid's daemon is
// Linux-only by construction (userns, pivot_root, the container backend's
// docker-out-of-docker wiring — see docs/plans/windows-client-build.md for
// why the CLIENT half is nevertheless portable).
//
// A non-Linux boid is a REMOTE client: it reaches a daemon running on a
// Linux host over the same authenticated listener the Web UI uses, which
// `boid login <url>` sets up. That path never calls ensureRunning at all,
// so this stub is unreachable in normal use — it exists to turn what would
// otherwise be a confusing "boid start: exec format error" deep inside
// spawnServer into one sentence naming the actual fix.
func lockAutostart(*os.File) (func(), error) {
	return nil, fmt.Errorf("cannot autostart a local boid daemon on %s: the daemon is linux-only; pair this CLI with a remote daemon using 'boid login <url>'", runtime.GOOS)
}
