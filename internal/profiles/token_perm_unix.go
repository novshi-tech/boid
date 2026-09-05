//go:build !windows

package profiles

import (
	"io/fs"
	"log/slog"
	"os"
)

// warnIfTokenPermsLoose logs a warning when the token file's permission
// bits are looser than tokenFilePerm. Not a hard error — the token is
// still usable, and refusing to read it would turn a merely-suspicious
// filesystem state into an outage; `chmod 600` is adequate remediation
// once the operator is told.
func warnIfTokenPermsLoose(path string, mode fs.FileMode) {
	if perm := mode.Perm(); perm&^tokenFilePerm != 0 {
		slog.Warn("token file has looser permissions than required; run chmod 600",
			"path", path, "mode", perm.String(), "want", os.FileMode(tokenFilePerm).String())
	}
}

// TokenProtectionNote returns a caveat to show the user once, after a
// successful `boid login`, about how the stored token is protected. Empty
// here: on a POSIX system the 0600 bits WriteToken sets are the real
// protection and are enforced by the kernel, so there is nothing to
// disclaim. See the Windows counterpart for the case that needs one.
func TokenProtectionNote() string { return "" }
