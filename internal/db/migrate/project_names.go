package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// preserveProjectNames is the only remaining reader of repository project.yaml.
// It captures legacy identity once, inside the schema migration transaction;
// runtime configuration always comes from the workspace. A malformed or unreadable
// existing file aborts the migration rather than silently renaming the project.
func preserveProjectNames(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, work_dir FROM projects`)
	if err != nil {
		return err
	}
	type project struct{ id, dir string }
	var projects []project
	for rows.Next() {
		var p project
		if err := rows.Scan(&p.id, &p.dir); err != nil {
			rows.Close()
			return err
		}
		projects = append(projects, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, p := range projects {
		name, err := legacyProjectName(p.dir)
		if err != nil {
			return fmt.Errorf("preserve project %q name: %w", p.id, err)
		}
		if name == "" && p.dir != "" {
			name = strings.TrimSuffix(filepath.Base(p.dir), ".git")
		}
		if _, err := tx.Exec(`UPDATE projects SET name=? WHERE id=?`, name, p.id); err != nil {
			return err
		}
	}
	return nil
}

func legacyProjectName(dir string) (string, error) {
	if dir == "" {
		return "", nil
	}
	path := filepath.Join(dir, ".boid", "project.yaml")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		// Bare mirrors have no working tree: read the same HEAD path used by the
		// previous loader. A missing mirror or absent file has no legacy name.
		if _, statErr := os.Stat(filepath.Join(dir, "HEAD")); os.IsNotExist(statErr) {
			return "", nil
		} else if statErr != nil {
			return "", statErr
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		listing, listErr := exec.CommandContext(ctx, "git", "--git-dir="+dir, "ls-tree", "HEAD", "--", ".boid/project.yaml").Output()
		if listErr != nil {
			return "", fmt.Errorf("read mirror HEAD: %w", listErr)
		}
		if len(listing) == 0 {
			return "", nil
		}
		data, err = exec.CommandContext(ctx, "git", "--git-dir="+dir, "show", "HEAD:.boid/project.yaml").Output()
	}
	if err != nil {
		return "", err
	}
	var identity struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(data, &identity); err != nil {
		return "", fmt.Errorf("decode legacy name: %w", err)
	}
	return identity.Name, nil
}
