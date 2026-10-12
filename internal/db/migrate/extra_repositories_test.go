package migrate

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
)

func TestExtraRepositoriesMigrationCreatesFetchRows(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	applyThrough(t, d.Conn, "0062_persist_project_names")
	if _, err := d.Conn.Exec(`INSERT INTO workspaces(slug,extra_repos) VALUES ('team','["git@github.com:org/lib.git","https://github.com/org/lib","https://github.com/other/lib.git","bad","https://user:secret@github.com/org/secret.git"]')`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := d.Conn.QueryRow(`SELECT count(*) FROM projects p JOIN project_workspaces pw ON pw.project_id=p.id WHERE pw.workspace_id='team' AND p.perm='fetch' AND p.work_dir='' AND p.upstream_url!=''`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("fetch rows=%d err=%v", count, err)
	}
	var legacy string
	if err := d.Conn.QueryRow(`SELECT extra_repos FROM workspaces WHERE slug='team'`).Scan(&legacy); err != nil || legacy != "[]" {
		t.Fatalf("legacy=%q err=%v", legacy, err)
	}
	if err := Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
}

func TestExtraRepositoriesMigrationPreservesExistingPushPermission(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	applyThrough(t, d.Conn, "0062_persist_project_names")
	if _, err := d.Conn.Exec(`INSERT INTO workspaces(slug,extra_repos) VALUES ('team','["git@github.com:org/lib.git"]'); INSERT INTO projects(id,name,work_dir,upstream_url,perm) VALUES ('legacy','Library','/old/mirror','https://github.com/org/lib.git','push')`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	var id, perm, workspace string
	if err := d.Conn.QueryRow(`SELECT p.id,p.perm,pw.workspace_id FROM projects p JOIN project_workspaces pw ON pw.project_id=p.id`).Scan(&id, &perm, &workspace); err != nil || id != "legacy" || perm != "push" || workspace != "team" {
		t.Fatalf("%q %q %q %v", id, perm, workspace, err)
	}
}

func TestExtraRepositoriesMigrationPreservesOtherWorkspaceAndNames(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	applyThrough(t, d.Conn, "0062_persist_project_names")
	if _, err := d.Conn.Exec(`INSERT INTO workspaces(slug,extra_repos) VALUES ('owner','[]'),('team','["https://github.com/org/lib.git","https://github.com/org/lib.git"]'); INSERT INTO projects(id,name,work_dir,upstream_url,perm) VALUES ('url-existing','lib','/old/mirror','https://github.com/org/lib.git','push'),('no-url','Unknown','/missing',NULL,'push'); INSERT INTO project_workspaces(project_id,workspace_id) VALUES ('url-existing','owner')`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	rows, err := d.Conn.Query(`SELECT p.id,p.name,p.perm,pw.workspace_id FROM projects p JOIN project_workspaces pw ON pw.project_id=p.id ORDER BY pw.workspace_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	names := map[string]bool{}
	count := 0
	for rows.Next() {
		var id, name, perm, workspace string
		if err := rows.Scan(&id, &name, &perm, &workspace); err != nil {
			t.Fatal(err)
		}
		count++
		if names[name] {
			t.Fatalf("ambiguous registration name %q", name)
		}
		names[name] = true
		if workspace == "owner" && (id != "url-existing" || perm != "push") {
			t.Fatal("existing assignment changed")
		}
		if workspace == "team" && (!strings.HasPrefix(id, "extra-") || perm != "fetch" || name == "lib") {
			t.Fatalf("migrated row: %s %s %s", id, name, perm)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("rows=%d", count)
	}
}

func TestExtraRepositoriesMigrationSkipsMalformedList(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	applyThrough(t, d.Conn, "0062_persist_project_names")
	if _, err := d.Conn.Exec(`INSERT INTO workspaces(slug,extra_repos) VALUES ('team','{broken')`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
}

func TestExtraRepositoriesMigrationResolvesIDAndNameCollisions(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	applyThrough(t, d.Conn, "0062_persist_project_names")
	sum := sha256.Sum256([]byte("team\x00https://github.com/org/lib.git"))
	suffix := hex.EncodeToString(sum[:])[:16]
	if _, err := d.Conn.Exec(`INSERT INTO workspaces(slug,extra_repos) VALUES ('team','["https://github.com/org/lib.git"]'); INSERT INTO projects(id,name,work_dir,upstream_url,perm) VALUES (?, 'lib', '', 'https://github.com/other/existing.git','push'),('name-conflict',?,'','https://github.com/other/second.git','push')`, "extra-"+suffix, "lib-"+suffix+"-1"); err != nil {
		t.Fatal(err)
	}
	if err := Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	var id, name string
	if err := d.Conn.QueryRow(`SELECT id,name FROM projects WHERE perm='fetch'`).Scan(&id, &name); err != nil {
		t.Fatal(err)
	}
	if id != "extra-"+suffix+"-2" || name != "lib-"+suffix+"-2" {
		t.Fatalf("collision result: %s %s", id, name)
	}
}

func TestExtraRepositoriesMigrationPrefersWorkspaceOwnedRow(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	applyThrough(t, d.Conn, "0062_persist_project_names")
	if _, err := d.Conn.Exec(`INSERT INTO workspaces(slug,extra_repos) VALUES ('team','["https://github.com/org/lib.git"]'); INSERT INTO projects(id,name,work_dir,upstream_url,perm) VALUES ('a-unassigned','unassigned','','https://github.com/org/lib.git','push'),('z-owned','owned','','https://github.com/org/lib.git','push'); INSERT INTO project_workspaces(project_id,workspace_id) VALUES ('z-owned','team')`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := d.Conn.QueryRow(`SELECT count(*) FROM project_workspaces WHERE workspace_id='team'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("workspace rows=%d err=%v", count, err)
	}
}

func TestExtraRepositoriesMigrationNormalizesProjectNames(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	applyThrough(t, d.Conn, "0062_persist_project_names")
	if _, err := d.Conn.Exec(`INSERT INTO workspaces(slug,extra_repos) VALUES ('team','["https://github.com/org/.github.git","https://github.com/org/a..b.git"]')`); err != nil {
		t.Fatal(err)
	}
	if err := Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := d.Conn.QueryRow(`SELECT count(*) FROM projects WHERE name IN ('github','a-b')`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("normalized names=%d err=%v", count, err)
	}
}
