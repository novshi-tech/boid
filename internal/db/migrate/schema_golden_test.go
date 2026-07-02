package migrate

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
)

// updateGolden regenerates testdata/schema.golden instead of comparing against
// it. Run: go test ./internal/db/migrate -run TestSchemaGolden -update-golden
var updateGolden = flag.Bool("update-golden", false, "rewrite the schema golden file")

const schemaGoldenPath = "testdata/schema.golden"

// TestSchemaGolden is Tier 1 #4 of docs/plans/quality-gates.md. Migrations are
// forward-only and their per-migration verification is limited to hand-written
// columnExists / tableExists preflight checks — a migration that forgets its
// assertion still applies silently. This test applies the full migration chain
// to a fresh in-memory DB and compares the resulting schema (the CREATE
// statements sqlite records in sqlite_master) against a checked-in golden file.
// Any migration that changes a table/index/trigger/view now surfaces as a diff,
// and the author is forced to regenerate the golden with -update-golden — making
// unintended schema drift impossible to merge unnoticed.
//
// There is no down path (apply-only design), so rollback is out of scope.
func TestSchemaGolden(t *testing.T) {
	got := applyAndDumpSchema(t)

	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(schemaGoldenPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(schemaGoldenPath, []byte(got), 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s (%d bytes)", schemaGoldenPath, len(got))
		return
	}

	want, err := os.ReadFile(schemaGoldenPath)
	if err != nil {
		t.Fatalf("read golden (%s): %v\nRun: go test ./internal/db/migrate -run TestSchemaGolden -update-golden", schemaGoldenPath, err)
	}
	if got != string(want) {
		t.Errorf("post-migration schema differs from %s.\n"+
			"If this change is intentional, regenerate with:\n"+
			"    go test ./internal/db/migrate -run TestSchemaGolden -update-golden\n"+
			"and review the golden diff.\n\n--- got ---\n%s", schemaGoldenPath, got)
	}
}

// TestMigrationFilesAllWired guards the other direction: every migrations/*.sql
// file must actually be applied by Apply(). A file dropped into the directory
// but never wired into the Apply() slice would silently do nothing; comparing
// the embedded files against the versions Apply() records in schema_migrations
// catches that without needing Apply's private slice.
func TestMigrationFilesAllWired(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	// Files present on disk (embedded).
	entries, err := schemaFS.ReadDir("migrations")
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	files := make(map[string]bool)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		files[strings.TrimSuffix(name, ".sql")] = true
	}

	// Versions Apply() recorded.
	applied, err := appliedVersions(d.Conn)
	if err != nil {
		t.Fatalf("applied versions: %v", err)
	}

	for f := range files {
		if _, ok := applied[f]; !ok {
			t.Errorf("migration file %q.sql exists but is not wired into Apply() (never applied)", f)
		}
	}
	for v := range applied {
		if !files[v] {
			t.Errorf("Apply() references version %q but migrations/%s.sql does not exist", v, v)
		}
	}
}

// applyAndDumpSchema applies all migrations to a fresh in-memory DB and returns
// a deterministic dump of the resulting schema objects.
func applyAndDumpSchema(t *testing.T) string {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()
	if err := Apply(d.Conn); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	// sqlite_master.sql holds the CREATE text for every authored object; NULL
	// sql rows are auto-created indexes (UNIQUE/PK) which are derived, and the
	// sqlite_% names are internal. Ordering by (type, name) is stable.
	rows, err := d.Conn.Query(`
		SELECT type, name, sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		ORDER BY type, name`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var typ, name, sqlText string
		if err := rows.Scan(&typ, &name, &sqlText); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		b.WriteString(fmt.Sprintf("-- %s: %s\n", typ, name))
		b.WriteString(strings.TrimSpace(sqlText))
		b.WriteString(";\n\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	return b.String()
}
