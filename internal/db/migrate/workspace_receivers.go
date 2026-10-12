package migrate

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// retireWorkspaceReceivers preserves repository-free records before deleting managed projects.
func retireWorkspaceReceivers(tx *sql.Tx) error {
	receivers, err := findWorkspaceReceivers(tx)
	if err != nil {
		return err
	}
	for _, r := range receivers {
		var workspaces int
		if err := tx.QueryRow(`SELECT count(*) FROM workspaces WHERE slug=?`, r.workspace).Scan(&workspaces); err != nil {
			return err
		}
		if workspaces != 1 {
			return fmt.Errorf("workspace receiver migration: missing workspace %s; repair before retrying", r.workspace)
		}
		if err := checkReceiverReferences(tx, r.id); err != nil {
			return err
		}
		for _, table := range []string{"task_identities", "tasks", "jobs", "trigger_runs"} {
			var mismatches int
			if err := tx.QueryRow(`SELECT count(*) FROM `+table+` WHERE project_id=? AND workspace_id != ?`, r.id, r.workspace).Scan(&mismatches); err != nil {
				return err
			}
			if mismatches != 0 {
				return fmt.Errorf("workspace receiver migration: project %s has %d %s workspace mismatches; repair before retrying", r.id, mismatches, table)
			}
			if table == "task_identities" {
				if err := tx.QueryRow(`SELECT count(*) FROM task_identities i JOIN tasks t ON t.id=i.task_id WHERE i.project_id=? AND COALESCE(t.project_id,'') != ?`, r.id, r.id).Scan(&mismatches); err != nil {
					return err
				}
				if mismatches != 0 {
					return fmt.Errorf("workspace receiver migration: identity/task repository mismatch for %s", r.id)
				}
			}
			if _, err := tx.Exec(`UPDATE `+table+` SET project_id=NULL WHERE project_id=?`, r.id); err != nil {
				return fmt.Errorf("workspace receiver migration: %s: %w", table, err)
			}
		}
		if _, err := tx.Exec(`DELETE FROM project_workspaces WHERE project_id=?`, r.id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM projects WHERE id=?`, r.id); err != nil {
			return err
		}
	}
	return nil
}

func validateReceiverIdentities(tx *sql.Tx) error {
	var orphaned int
	if err := tx.QueryRow(`SELECT count(*) FROM task_identities i LEFT JOIN tasks t ON t.id=i.task_id WHERE t.id IS NULL`).Scan(&orphaned); err != nil {
		return err
	}
	if orphaned != 0 {
		return fmt.Errorf("workspace receiver migration: %d identity bindings have no task; repair before retrying", orphaned)
	}
	receivers, err := findWorkspaceReceivers(tx)
	if err != nil {
		return err
	}
	for _, r := range receivers {
		if err := checkReceiverReferences(tx, r.id); err != nil {
			return err
		}
	}
	return nil
}

// checkReceiverReferences refuses unhandled references that could be lost on deletion.
func checkReceiverReferences(tx *sql.Tx, projectID string) error {
	rows, err := tx.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return err
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			return err
		}
		tables = append(tables, name)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	handled := map[string]bool{"tasks": true, "jobs": true, "trigger_runs": true, "task_identities": true, "project_workspaces": true}
	for _, table := range tables {
		if handled[table] {
			continue
		}
		quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
		refs, err := tx.Query(`PRAGMA foreign_key_list(` + quoted + `)`)
		if err != nil {
			return err
		}
		columns := map[string]bool{}
		for refs.Next() {
			var id, seq int
			var target, from, to, onUpdate, onDelete, match string
			if err := refs.Scan(&id, &seq, &target, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				refs.Close()
				return err
			}
			if target == "projects" {
				columns[from] = true
			}
		}
		err = refs.Err()
		refs.Close()
		if err != nil {
			return err
		}
		exists, err := columnExists(tx, table, "project_id")
		if err != nil {
			return err
		}
		if exists {
			columns["project_id"] = true
		}
		for column := range columns {
			var n int
			if err := tx.QueryRow(`SELECT count(*) FROM `+quoted+` WHERE "`+strings.ReplaceAll(column, `"`, `""`)+`" = ?`, projectID).Scan(&n); err != nil {
				return err
			}
			if n != 0 {
				return fmt.Errorf("workspace receiver migration: unhandled reference %s.%s (%d rows) to %s; preserve these records before retrying", table, column, n, projectID)
			}
		}
	}
	return nil
}

type workspaceReceiver struct{ id, workspace string }

func findWorkspaceReceivers(tx *sql.Tx) ([]workspaceReceiver, error) {
	rows, err := tx.Query(`SELECT p.id,pw.workspace_id FROM projects p JOIN project_workspaces pw ON pw.project_id=p.id WHERE p.work_dir='' AND COALESCE(p.upstream_url,'')=''`)
	if err != nil {
		return nil, err
	}
	var receivers []workspaceReceiver
	for rows.Next() {
		var r workspaceReceiver
		if err := rows.Scan(&r.id, &r.workspace); err != nil {
			rows.Close()
			return nil, err
		}
		if r.workspace != "" && r.id == uuid.NewSHA1(uuid.NameSpaceURL, []byte("boid:default-metaproject:"+r.workspace)).String() {
			receivers = append(receivers, r)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	return receivers, nil
}
