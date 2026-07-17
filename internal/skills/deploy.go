package skills

import (
	"bytes"
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed data/boid-web data/boid-orchestrate data/boid-task
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
		return writeFileAtomic(dest, embedded, 0o644)
	})
}

// writeFileAtomic replaces dest's content with data via a sibling temp file
// + rename, so a mid-write crash (or a concurrent reader) never observes a
// partially written file at dest. The temp file is created in the same
// directory as dest so the rename is guaranteed to stay on one filesystem
// (a cross-device rename would fail).
func writeFileAtomic(dest string, data []byte, perm os.FileMode) (retErr error) {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(dest)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp file %q: %w", tmpPath, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file %q: %w", tmpPath, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("rename %q to %q: %w", tmpPath, dest, err)
	}
	cleanup = false
	return nil
}
