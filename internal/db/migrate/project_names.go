package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// preserveProjectNames captures legacy identity once during migration.
// Unavailable legacy names fall back to the repository basename.
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
			slog.Warn("project name migration: unable to read legacy name; using repository basename", "project", p.id, "work_dir", p.dir, "error", err)
			name = ""
		}
		name = strings.TrimSpace(name)
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
			return "", mirrorReadError("read mirror HEAD", listErr)
		}
		if len(listing) == 0 {
			return "", nil
		}
		data, err = exec.CommandContext(ctx, "git", "--git-dir="+dir, "show", "HEAD:.boid/project.yaml").Output()
		if err != nil {
			return "", mirrorReadError("read mirror project.yaml", err)
		}
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

func mirrorReadError(operation string, err error) error {
	if exitErr, ok := err.(*exec.ExitError); ok {
		return fmt.Errorf("%s: %w: %s", operation, err, strings.TrimSpace(string(exitErr.Stderr)))
	}
	return fmt.Errorf("%s: %w", operation, err)
}
