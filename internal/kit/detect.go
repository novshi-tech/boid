package kit

import (
	"os"
	"path/filepath"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// Detect reports whether kit is applicable to projectDir.
// Returns true if any path in kit.Detect.Files exists under projectDir (OR semantics).
// Returns false when kit.Detect is nil.
func Detect(projectDir string, kit orchestrator.KitMeta) bool {
	if kit.Detect == nil {
		return false
	}
	for _, name := range kit.Detect.Files {
		if _, err := os.Stat(filepath.Join(projectDir, name)); err == nil {
			return true
		}
	}
	return false
}
