package skills

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed data/boid-sandbox data/boid-supervisor data/boid-executor data/boid-web data/boid-orchestrate data/boid-task
var skillsFS embed.FS

// DeployAll extracts all embedded skill directories under baseDir.
// Each skill is deployed to baseDir/<skill-name>/.
// Files are only written when their content differs from the embedded version.
func DeployAll(baseDir string) error {
	entries, err := skillsFS.ReadDir("data")
	if err != nil {
		return fmt.Errorf("read skills dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if err := deploySkill(e.Name(), filepath.Join(baseDir, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// EmbeddedSkillNames returns the slugs of the embedded skill directories in
// stable lexical order. dispatcher uses it to compute the claude-side
// ~/.claude/skills/<name> bind targets without hard-coding the list.
func EmbeddedSkillNames() []string {
	entries, err := skillsFS.ReadDir("data")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

func deploySkill(name, targetDir string) error {
	prefix := "data/" + name
	return fs.WalkDir(skillsFS, prefix, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(prefix, path)
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
		existing, err := os.ReadFile(dest)
		if err == nil && bytes.Equal(existing, embedded) {
			return nil
		}
		return os.WriteFile(dest, embedded, 0o644)
	})
}
