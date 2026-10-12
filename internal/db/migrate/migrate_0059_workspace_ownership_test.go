package migrate

import (
	"database/sql"
	"fmt"
	"reflect"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
)

func TestWorkspaceOwnershipMigration(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	applyThrough(t, d.Conn, "0058_add_workspace_automation")
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.Conn.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO workspaces(slug) VALUES ('team'), ('default')`)
	exec(`INSERT INTO projects(id,work_dir,upstream_url) VALUES ('linked','/linked','https://example.com/repo'), ('orphan','/orphan',NULL)`)
	exec(`INSERT INTO project_workspaces VALUES ('linked','team')`)
	for _, p := range []string{"linked", "orphan"} {
		exec(`INSERT INTO tasks(id,type,project_id,title,status,behavior,traits,readonly,branch_prefix,base_branch,payload,instructions,auto_start,idempotency_key) VALUES (?, 'execution', ?, 'task', 'pending', 'implement', '[]', 0, 'feature', 'main', '{}', '[]', 1, ?)`, p, p, p)
		exec(`INSERT INTO tasks(id,type,project_id,title,status,kind,urgency,wake_task_id,suggestion_verb,detail) VALUES (?, 'card', ?, 'card', 'parked', 'issue', 'high', '', 'start', '{}')`, p+"-card", p)
		exec(`INSERT INTO jobs(id,task_id,project_id,output,terminal_title,card_id) VALUES (?, ?, ?, 'output','terminal',?)`, p, p, p, p+"-card")
		exec(`INSERT INTO trigger_runs(id,project_id,trigger_name,job_id,started_at,finished_at,exit_code) VALUES (?, ?, 'sweep', ?, '2026-10-01', '2026-10-02', 7)`, p, p, p)
	}
	exec(`INSERT INTO actions(id,task_id,type) VALUES ('action','linked','start')`)
	// Legacy databases can contain dangling or empty repository references.
	exec(`PRAGMA foreign_keys=OFF`)
	exec(`INSERT INTO tasks(id,type,project_id,title,status,behavior,traits,readonly,branch_prefix,base_branch,payload,instructions,auto_start) VALUES ('dangling-task','execution','missing','legacy','pending','implement','[]',0,'feature','main','{}','[]',0)`)
	exec(`INSERT INTO jobs(id,project_id) VALUES ('empty-project-job','')`)
	exec(`PRAGMA foreign_keys=ON`)
	exec(`INSERT INTO task_identities(identity,project_id,task_id,created_at) VALUES ('identity','linked','linked','2026-10-01')`)
	exec(`INSERT INTO card_force_release_barriers(card_id,created_at) VALUES ('linked-card','2026-10-01')`)
	tables := []string{"projects", "tasks", "jobs", "trigger_runs", "actions", "task_identities", "card_force_release_barriers", "project_workspaces", "workspaces"}
	before := map[string][][]any{}
	columns := map[string]string{}
	for _, table := range tables {
		columns[table] = migrationColumns(t, d.Conn, table)
		before[table] = migrationRows(t, d.Conn, table, columns[table])
	}
	indexes := migrationRows(t, d.Conn, "sqlite_master WHERE type = 'index' AND sql IS NOT NULL", "name, sql")
	if err := Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	if got := migrationRows(t, d.Conn, "sqlite_master WHERE type = 'index' AND sql IS NOT NULL", "name, sql"); !reflect.DeepEqual(got, indexes) {
		t.Errorf("indexes changed: got %v want %v", got, indexes)
	}
	for _, table := range tables {
		if got := migrationRows(t, d.Conn, table, columns[table]); !reflect.DeepEqual(got, before[table]) {
			t.Errorf("%s lost existing values: got %v want %v", table, got, before[table])
		}
	}
	for _, table := range []string{"tasks", "jobs", "trigger_runs"} {
		var bad int
		if err := d.Conn.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s WHERE workspace_id IS NULL OR workspace_id != CASE project_id WHEN 'linked' THEN 'team' ELSE 'default' END`, table)).Scan(&bad); err != nil {
			t.Fatal(err)
		}
		if bad != 0 {
			t.Errorf("%s: %d incorrect workspace assignments", table, bad)
		}
		if _, err := d.Conn.Exec("UPDATE " + table + " SET project_id='missing' WHERE id='linked'"); err == nil {
			t.Errorf("%s lost repository FK", table)
		}
		exec(fmt.Sprintf(`UPDATE %s SET project_id=NULL WHERE id='orphan'`, table))
		var project sql.NullString
		if err := d.Conn.QueryRow("SELECT project_id FROM " + table + " WHERE id='orphan'").Scan(&project); err != nil {
			t.Fatal(err)
		}
		if project.Valid {
			t.Errorf("%s project is not NULL", table)
		}
		if _, err := d.Conn.Exec("UPDATE " + table + " SET workspace_id='' WHERE id='linked'"); err == nil {
			t.Errorf("%s accepted empty workspace", table)
		}
		if _, err := d.Conn.Exec("UPDATE " + table + " SET workspace_id=NULL WHERE id='linked'"); err == nil {
			t.Errorf("%s accepted NULL workspace", table)
		}
	}
	if _, err := d.Conn.Exec(`UPDATE tasks SET status='parked' WHERE id='linked'`); err == nil {
		t.Error("lost task status constraint")
	}
	if _, err := d.Conn.Exec(`UPDATE tasks SET kind='issue' WHERE id='linked'`); err == nil {
		t.Error("lost task subtype constraint")
	}
	exec(`INSERT INTO tasks(id,type,project_id,workspace_id,title,status,kind,urgency,wake_task_id,suggestion_verb,detail) VALUES ('nullable-card','card',NULL,'team','card','parked','','','','','{}')`)
	exec(`INSERT INTO jobs(id,project_id,workspace_id) VALUES ('nullable-job',NULL,'team')`)
	exec(`INSERT INTO trigger_runs(id,project_id,workspace_id,trigger_name,started_at) VALUES ('nullable-run',NULL,'team','sweep','2026-10-01')`)
	for _, table := range []string{"tasks", "jobs", "trigger_runs"} {
		var n int
		if err := d.Conn.QueryRow("SELECT count(*) FROM " + table + " WHERE project_id IS NULL").Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("%s NULL rows = %d, want 2", table, n)
		}
	}
	var bad int
	if err := d.Conn.QueryRow(`SELECT count(*) FROM projects WHERE perm != 'push'`).Scan(&bad); err != nil {
		t.Fatal(err)
	}
	if bad != 0 {
		t.Fatal("existing permissions not push")
	}
	exec(`UPDATE projects SET perm='fetch' WHERE id='orphan'`)
	if _, err := d.Conn.Exec(`UPDATE projects SET perm='invalid'`); err == nil {
		t.Fatal("invalid permission accepted")
	}
	// Remove the two pre-existing FK violations before checking new violations.
	exec(`DELETE FROM tasks WHERE id='dangling-task'`)
	exec(`DELETE FROM jobs WHERE id='empty-project-job'`)
	rows, err := d.Conn.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("foreign key violations")
	}
	rows.Close()
	if err := Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
}

func migrationColumns(t *testing.T, conn *sql.DB, table string) string {
	t.Helper()
	rows, err := conn.Query("SELECT * FROM " + table + " LIMIT 0")
	if err != nil {
		t.Fatal(err)
	}
	cols, err := rows.Columns()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	result := ""
	for i, c := range cols {
		if i > 0 {
			result += ","
		}
		result += c
	}
	return result
}

func migrationRows(t *testing.T, conn *sql.DB, table, columns string) [][]any {
	t.Helper()
	rows, err := conn.Query("SELECT " + columns + " FROM " + table + " ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var result [][]any
	for rows.Next() {
		values := make([]any, len(cols))
		dest := make([]any, len(cols))
		for i := range values {
			dest[i] = &values[i]
		}
		if err := rows.Scan(dest...); err != nil {
			t.Fatal(err)
		}
		result = append(result, values)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
