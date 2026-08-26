package conformance

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenInternalImportSubstring is the boid module's own internal
// import path prefix. Its appearance anywhere inside a Pack directory
// (source code, a README, whatever) is a red flag that someone is trying
// to reference boid internals a connector has no legitimate reason to know
// about — Q16-18's "拡張禁止": connector code cannot reach boid's state
// machine or DB. That is structurally enforced by the sandbox (a
// connector's job has no code path back into the daemon at all — the
// sandbox provides no such handle, matching docs/plans/signal-ingest-
// detailed-design.md §5.2's reduced connector-job policy), not by this
// check; this is only a light early-warning grep.
const forbiddenInternalImportSubstring = "novshi-tech/boid/internal"

// maxScannedFileSize caps how much of a file this check reads — a light
// guard has no business choking on a multi-megabyte binary asset a Pack
// happens to ship.
const maxScannedFileSize = 4 << 20 // 4 MiB

// findExtensionViolations is the pure detection half of the "拡張禁止
// (Q16-18 相当)" row: a coarse grep guard against a Pack shipping Go source
// (which has no business inside a Pack directory meant to be bind-mounted
// read-only into a job sandbox — docs/plans/signal-ingest-detailed-
// design.md §7.1's mount contract) or a literal reference to boid's own
// internal/ import path. Deliberately light ("過剰検査は避ける") — real
// enforcement here is structural, this is only a sanity trip-wire.
//
// Separated from checkNoExtensionEscape's *testing.T reporting for the
// same reason as findSkillDocViolations (see its own doc comment).
func findExtensionViolations(dir string) ([]string, error) {
	var violations []string
	walkErr := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			rel = path
		}
		if strings.EqualFold(filepath.Ext(d.Name()), ".go") {
			violations = append(violations, fmt.Sprintf("%s: Pack directory must not ship .go source files (Q16-18: a Pack has no legitimate reason to contain boid extension code)", rel))
		}
		info, err := d.Info()
		if err != nil || info.Size() == 0 || info.Size() > maxScannedFileSize {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			// Best-effort: an unreadable file (permissions, a symlink race,
			// ...) is not this check's concern.
			return nil
		}
		if strings.Contains(string(data), forbiddenInternalImportSubstring) {
			violations = append(violations, fmt.Sprintf("%s: references %q — a Pack must not depend on boid's internal packages", rel, forbiddenInternalImportSubstring))
		}
		return nil
	})
	return violations, walkErr
}

// checkNoExtensionEscape is findExtensionViolations' *testing.T reporter.
func checkNoExtensionEscape(t *testing.T, dir string) {
	t.Helper()
	violations, err := findExtensionViolations(dir)
	if err != nil {
		t.Errorf("walk %s: %v", dir, err)
	}
	for _, v := range violations {
		t.Error(v)
	}
}
