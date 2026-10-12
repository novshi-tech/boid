package migrate

import (
	"database/sql"
	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/db"
	"reflect"
	"strings"
	"testing"
)

func receiverFixture(t *testing.T) (*sql.DB, string) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	applyThrough(t, d.Conn, "0060_workspace_task_dedup")
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("boid:default-metaproject:team")).String()
	queries := []struct {
		q    string
		args []any
	}{
		{`INSERT INTO workspaces(slug) VALUES ('team'),('other')`, nil},
		{`INSERT INTO projects(id,work_dir) VALUES (?, '')`, []any{id}},
		{`INSERT INTO project_workspaces(project_id,workspace_id) VALUES (?,'team')`, []any{id}},
		{`INSERT INTO tasks(id,type,project_id,workspace_id,title,status,kind,urgency,wake_task_id,suggestion_verb,detail) VALUES ('card','card',?,'team','Card','parked','','','','','{}')`, []any{id}},
		{`INSERT INTO tasks(id,type,project_id,workspace_id,title,status,behavior,traits,readonly,branch_prefix,base_branch,payload,instructions,auto_start,parent_id) VALUES ('judge','execution',?,'team','Judge','executing','judge','[]',1,'','','{}','[]',1,'card')`, []any{id}},
		{`INSERT INTO jobs(id,task_id,project_id,workspace_id,card_id,card_request_id,output) VALUES ('job','judge',?,'team','card','request','history')`, []any{id}},
		{`INSERT INTO trigger_runs(id,project_id,workspace_id,trigger_name,job_id,started_at,finished_at,exit_code) VALUES ('run',?,'team','sweep','job','2026-10-01','2026-10-02',7)`, []any{id}},
		{`INSERT INTO actions(id,task_id,type,payload) VALUES ('action','card','noted','{"body":"history"}')`, nil},
		{`INSERT INTO task_identities(identity,project_id,task_id,created_at,url,display_name) VALUES ('jira:1',?,'card','2026-10-01','https://example.com','Issue')`, []any{id}},
		{`INSERT INTO card_requests(id,card_id,command_key,cause_id,created_at,updated_at) VALUES ('request','card','judge','action','2026-10-01','2026-10-01')`, nil},
		{`INSERT INTO card_force_release_barriers(card_id,created_at) VALUES ('card','2026-10-01')`, nil},
	}
	for _, q := range queries {
		if _, err := d.Conn.Exec(q.q, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	return d.Conn, id
}

func TestWorkspaceReceiverMigrationPreservesAllRecords(t *testing.T) {
	conn, id := receiverFixture(t)
	preserved := map[string][][]any{}
	for _, table := range []string{"actions", "card_requests", "card_force_release_barriers"} {
		preserved[table] = migrationRows(t, conn, table, migrationColumns(t, conn, table))
	}
	before := map[string][][]any{}
	cols := map[string]string{}
	for _, table := range []string{"tasks", "jobs", "trigger_runs", "task_identities"} {
		cols[table] = migrationColumns(t, conn, table)
		before[table] = migrationRows(t, conn, table, cols[table])
		for _, row := range before[table] {
			for i, column := range strings.Split(cols[table], ",") {
				if column == "project_id" {
					row[i] = nil
				}
			}
		}
	}
	if err := Apply(conn); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"tasks", "jobs", "trigger_runs", "task_identities"} {
		var bad, total int
		if err := conn.QueryRow(`SELECT count(*),count(CASE WHEN project_id IS NOT NULL OR workspace_id != 'team' THEN 1 END) FROM `+table).Scan(&total, &bad); err != nil {
			t.Fatal(err)
		}
		want := 1
		if table == "tasks" {
			want = 2
		}
		if total != want || bad != 0 {
			t.Fatalf("%s total=%d bad=%d", table, total, bad)
		}
	}
	for table, want := range before {
		if got := migrationRows(t, conn, table, cols[table]); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s changed surviving values: got=%v want=%v", table, got, want)
		}
	}
	for table, want := range preserved {
		got := migrationRows(t, conn, table, migrationColumns(t, conn, table))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s lost data", table)
		}
	}
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM projects WHERE id=?`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("receiver remains: %d %v", n, err)
	}
	if err := conn.QueryRow(`SELECT count(*) FROM project_workspaces WHERE project_id=?`, id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("membership remains: %d %v", n, err)
	}
	rows, err := conn.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("FK violation")
	}
	if err := Apply(conn); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceReceiverMigrationRejectsMismatchAtomically(t *testing.T) {
	for _, table := range []string{"tasks", "jobs", "trigger_runs"} {
		t.Run(table, func(t *testing.T) {
			conn, id := receiverFixture(t)
			if _, err := conn.Exec(`UPDATE ` + table + ` SET workspace_id='other'`); err != nil {
				t.Fatal(err)
			}
			err := Apply(conn)
			if err == nil || !strings.Contains(err.Error(), "workspace mismatches") {
				t.Fatalf("migration error: %v", err)
			}
			var n int
			if err := conn.QueryRow(`SELECT count(*) FROM projects WHERE id=?`, id).Scan(&n); err != nil || n != 1 {
				t.Fatalf("receiver lost: %d %v", n, err)
			}
			if err := conn.QueryRow(`SELECT count(*) FROM trigger_runs WHERE project_id=?`, id).Scan(&n); err != nil || n != 1 {
				t.Fatalf("history lost: %d %v", n, err)
			}
			exists, err := columnExists(conn, "task_identities", "workspace_id")
			if err != nil || exists {
				t.Fatalf("schema not rolled back: %v %v", exists, err)
			}
		})
	}
}

func TestWorkspaceReceiverMigrationLeavesUnmanagedSameID(t *testing.T) {
	for _, column := range []string{"work_dir", "upstream_url"} {
		t.Run(column, func(t *testing.T) {
			conn, id := receiverFixture(t)
			if _, err := conn.Exec(`UPDATE projects SET `+column+`='user-owned' WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
			if err := Apply(conn); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := conn.QueryRow(`SELECT count(*) FROM projects WHERE id=?`, id).Scan(&n); err != nil || n != 1 {
				t.Fatalf("unmanaged project removed: %d %v", n, err)
			}
			if err := conn.QueryRow(`SELECT count(*) FROM trigger_runs WHERE project_id=?`, id).Scan(&n); err != nil || n != 1 {
				t.Fatalf("unmanaged history lost: %d %v", n, err)
			}
		})
	}
}

func TestWorkspaceReceiverMigrationRejectsUnexpectedReference(t *testing.T) {
	conn, id := receiverFixture(t)
	if _, err := conn.Exec(`CREATE TABLE surprise (receiver_id TEXT REFERENCES projects(id) ON DELETE CASCADE)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO surprise VALUES (?)`, id); err != nil {
		t.Fatal(err)
	}
	if err := Apply(conn); err == nil || !strings.Contains(err.Error(), "unhandled reference surprise.receiver_id") {
		t.Fatalf("error: %v", err)
	}
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM surprise`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unexpected data lost: %d %v", n, err)
	}
}
func TestWorkspaceReceiverMigrationRejectsOrphanedIdentity(t *testing.T) {
	conn, id := receiverFixture(t)
	if _, err := conn.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO task_identities(identity,project_id,task_id,created_at) VALUES ('orphan',?,'missing','2026-10-01')`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(conn); err == nil || !strings.Contains(err.Error(), "identity bindings have no task") {
		t.Fatalf("error: %v", err)
	}
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM task_identities`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("identity lost: %d %v", n, err)
	}
}

func TestWorkspaceReceiverMigrationRejectsLiveTriggerCollision(t *testing.T) {
	conn, id := receiverFixture(t)
	if _, err := conn.Exec(`UPDATE trigger_runs SET finished_at=NULL,exit_code=NULL`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(`INSERT INTO trigger_runs(id,project_id,workspace_id,trigger_name,started_at) VALUES ('workspace-run',NULL,'team','sweep','2026-10-03')`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(conn); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed") {
		t.Fatalf("error: %v", err)
	}
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM trigger_runs`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("trigger history lost: %d %v", n, err)
	}
	if err := conn.QueryRow(`SELECT count(*) FROM projects WHERE id=?`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("receiver lost: %d %v", n, err)
	}
}

func TestWorkspaceReceiverMigrationLeavesMissingMembership(t *testing.T) {
	conn, id := receiverFixture(t)
	if _, err := conn.Exec(`DELETE FROM project_workspaces WHERE project_id=?`, id); err != nil {
		t.Fatal(err)
	}
	if err := Apply(conn); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM projects WHERE id=?`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("unmanaged project removed: %d %v", n, err)
	}
}
