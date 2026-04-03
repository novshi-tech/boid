package skills

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed data/SKILL.md data/references
var skillsFS embed.FS

// Deploy extracts the embedded boid-sandbox skill files to targetDir.
// Files are only written when their content differs from the embedded version.
func Deploy(targetDir string) error {
	return fs.WalkDir(skillsFS, "data", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Strip "data/" prefix to get the relative output path
		rel, err := filepath.Rel("data", path)
		if err != nil {
			return fmt.Errorf("rel path: %w", err)
		}
		dest := filepath.Join(targetDir, rel)

		if d.IsDir() {
			return os.MkdirAll(dest, 0o755)
		}

		embedded, err := skillsFS.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read embedded %s: %w", path, err)
		}

		// Skip write if content is identical
		existing, err := os.ReadFile(dest)
		if err == nil && bytes.Equal(existing, embedded) {
			return nil
		}

		return os.WriteFile(dest, embedded, 0o644)
	})
}
