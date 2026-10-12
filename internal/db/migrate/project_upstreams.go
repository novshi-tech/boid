package migrate

import (
	"context"
	"database/sql"
	"log/slog"
	"os/exec"
	"time"

	"github.com/novshi-tech/boid/internal/db/legacyrepos"
)

// recoverProjectUpstreams reads missing legacy URLs once without contacting a remote.
func recoverProjectUpstreams(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id,work_dir FROM projects WHERE upstream_url IS NULL OR upstream_url=''`)
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
		if p.dir == "" {
			slog.Warn("upstream migration: no legacy mirror; skipping", "project", p.id)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		raw, err := exec.CommandContext(ctx, "git", "--git-dir="+p.dir, "config", "--local", "--get", "remote.origin.url").Output()
		cancel()
		if err != nil {
			slog.Warn("upstream migration: unable to read local origin; skipping", "project", p.id)
			continue
		}
		upstream, err := legacyrepos.NormalizeRepositoryURL(string(raw))
		if err != nil {
			slog.Warn("upstream migration: invalid local origin; skipping", "project", p.id, "error", err)
			continue
		}
		if _, err := tx.Exec(`UPDATE projects SET upstream_url=? WHERE id=?`, upstream, p.id); err != nil {
			return err
		}
	}
	return nil
}
