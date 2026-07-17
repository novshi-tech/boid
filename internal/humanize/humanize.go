// Package humanize renders byte counts as human-readable strings and sums a
// directory tree's on-disk size, for boid's workspace-home size reporting
// (docs/plans/home-workspace-volume.md Phase 4 PR5: `boid workspace show`,
// `boid gc`'s workspace_homes listing, and the confirmation prompt on `boid
// workspace remove`).
package humanize

import (
	"fmt"
	"os"
	"path/filepath"
)

// SI (decimal, 1000-based) unit thresholds — chosen over IEC (1024-based
// KiB/MiB/...) because this package exists purely for human-facing CLI
// output, where the more familiar decimal units read naturally (see
// docs/plans/home-workspace-volume.md PR5 brief's explicit call: "SI 単位を
// 採用 (KB/MB/GB) — user 向け表示なので自然な数字が読みやすい"). Byte counts
// themselves stay int64 everywhere in boid; these constants only apply at
// display time.
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

// DirSize returns the total size, in bytes, of every regular file found by
// recursively walking root (a `du`-equivalent sum) — directory entries
// themselves do not contribute to the total, only file content sizes do.
// Symlinks are not followed (filepath.Walk uses Lstat internally), so a
// symlink's own directory-entry size is counted but nothing on the far side
// of it is descended into — this avoids both symlink loops and a home
// directory's size silently including content that lives outside it.
//
// Any error encountered while walking — root itself missing, a permission
// error partway through a subdirectory, or anything else Walk's callback
// receives — aborts the walk immediately and is returned verbatim; callers
// must treat a non-nil error as "size unknown" (docs/plans/
// home-workspace-volume.md PR5: "エラー時はエラーにせず「?」表示にして
// continue") rather than trusting the also-returned partial total.
func DirSize(root string) (int64, error) {
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
