// Package humanize renders byte counts as human-readable strings and sums a
// directory tree's apparent (logical) size, for boid's workspace-home size
// reporting (`boid workspace show`, `boid gc`'s workspace_homes listing, and
// the confirmation prompt on `boid workspace remove`).
//
// FormatBytes is what those three still use. ApparentSize is not — sizing
// moved onto the docker engine since a workspace home is a volume the daemon
// cannot walk directly; see ApparentSize's own doc comment for its current
// status.
package humanize

import (
	"fmt"
	"os"
	"path/filepath"
)

// SI (decimal, 1000-based) unit thresholds — chosen over IEC (1024-based
// KiB/MiB/...) since this package exists purely for human-facing CLI output,
// where decimal units read more naturally. Byte counts themselves stay
// int64 everywhere in boid; these constants only apply at display time.
const (
	unitKB = 1000
	unitMB = unitKB * 1000
	unitGB = unitMB * 1000
	unitTB = unitGB * 1000
	unitPB = unitTB * 1000
)

// FormatBytes renders n as a human-readable SI byte size: a plain integer
// count below 1000 ("500 B", "0 B"), or "%.2f <unit>" (two decimal places)
// scaled to the largest unit that keeps the leading digit non-zero (KB, MB,
// GB, TB, PB — boid workspace homes are not expected to reach EB). Negative
// values (should not occur — callers only ever pass a summed directory
// size) are treated as 0 B rather than producing a nonsensical "-1 B".
func FormatBytes(n int64) string {
	switch {
	case n < 0:
		return "0 B"
	case n < unitKB:
		return fmt.Sprintf("%d B", n)
	case n < unitMB:
		return formatUnit(n, unitKB, "KB")
	case n < unitGB:
		return formatUnit(n, unitMB, "MB")
	case n < unitTB:
		return formatUnit(n, unitGB, "GB")
	case n < unitPB:
		return formatUnit(n, unitTB, "TB")
	default:
		return formatUnit(n, unitPB, "PB")
	}
}

func formatUnit(n, unit int64, suffix string) string {
	return fmt.Sprintf("%.2f %s", float64(n)/float64(unit), suffix)
}

// ApparentSize has NO production caller any more — sizing a live workspace
// home moved onto the docker engine's GET /system/df, since the daemon
// cannot walk a volume's contents from outside a container. It is kept for
// `boid workspace import-home`, which still sizes legacy pre-migration
// <runtimesRoot>/homes/<slug> directories on disk.
//
// ApparentSize returns the total *apparent* (logical) size, in bytes, of
// every regular file found by recursively walking root — directory entries
// themselves do not contribute to the total, only file content sizes do.
// Symlinks are not followed (filepath.Walk uses Lstat internally), so a
// symlink's own directory-entry size is counted but nothing on the far side
// of it is descended into — this avoids both symlink loops and a home
// directory's size silently including content that lives outside it.
//
// This is a plain sum of FileInfo.Size() and is *not* a `du`-equivalent
// block-based measurement: it deliberately matches `du --apparent-size`, not
// plain `du`. Two known, accepted trade-offs follow: a sparse file's logical
// size is counted even though it occupies far fewer disk blocks, and a
// hardlinked file is counted once per name found rather than once per
// inode. Apparent size is judged good enough for boid's "is this workspace
// home suspiciously large" visibility use case.
//
// Any error encountered while walking aborts the walk immediately and is
// returned verbatim; callers must treat a non-nil error as "size unknown"
// rather than trusting the also-returned partial total.
func ApparentSize(root string) (int64, error) {
	var total int64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}
