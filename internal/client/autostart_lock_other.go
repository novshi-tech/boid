//go:build !linux

package client

import (
	"fmt"
	"os"
	"runtime"
)

// lockAutostart fails closed on every non-Linux build: reaching it means
// the CLI was about to spawn a local daemon, and boid's daemon is
// Linux-only by construction. A non-Linux boid is a REMOTE client instead
// (reaching a Linux daemon via `boid login <url>`), so this path never
// calls ensureRunning in normal use — it exists to turn a confusing
// low-level exec failure into one sentence naming the actual fix.
func lockAutostart(*os.File) (func(), error) {
	return nil, fmt.Errorf("cannot autostart a local boid daemon on %s: the daemon is linux-only; pair this CLI with a remote daemon using 'boid login <url>'", runtime.GOOS)
}
