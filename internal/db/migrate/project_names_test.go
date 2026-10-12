package migrate

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
)

func TestProjectNamesMigrationPreservesLegacyIdentityOnce(t *testing.T) {
	for _, bare := range []bool{false, true} {
		t.Run(map[bool]string{false: "checkout", true: "mirror"}[bare], func(t *testing.T) {
			dir := t.TempDir()
			work := filepath.Join(dir, "my-service")
			if err := os.MkdirAll(filepath.Join(work, ".boid"), 0755); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(work, ".boid", "project.yaml")
			if err := os.WriteFile(file, []byte("id: p\nname: svc\nbase_branch: legacy\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if bare {
				git := func(args ...string) {
					t.Helper()
					cmd := exec.Command("git", args...)
					cmd.Dir = work
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git: %v %s", err, out)
					}
				}
				git("init", "-q", "-b", "main")
				git("-c", "user.name=Test", "-c", "user.email=test@example.com", "add", ".")
				git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-qm", "legacy")
				mirror := filepath.Join(dir, "my-service.git")
				git("clone", "-q", "--bare", work, mirror)
				work = mirror
			}
			d, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			// Apply the actual pre-upgrade schema before inserting the legacy row.
			applyThrough(t, d.Conn, "0061_workspace_receivers")
			if _, err := d.Conn.Exec(`INSERT INTO projects(id,work_dir) VALUES ('p',?)`, work); err != nil {
				t.Fatal(err)
			}
			if err := Apply(d.Conn); err != nil {
				t.Fatal(err)
			}
			var name string
			if err := d.Conn.QueryRow(`SELECT name FROM projects WHERE id='p'`).Scan(&name); err != nil || name != "svc" {
				t.Fatalf("name=%q err=%v", name, err)
			}
			if err := os.RemoveAll(work); err != nil {
				t.Fatal(err)
			}
			if err := Apply(d.Conn); err != nil {
				t.Fatal(err)
			}
			if err := d.Conn.QueryRow(`SELECT name FROM projects WHERE id='p'`).Scan(&name); err != nil || name != "svc" {
				t.Fatalf("name changed on restart: %q %v", name, err)
			}
		})
	}
}

func TestProjectNamesMigrationRejectsMalformedLegacyIdentity(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".boid"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".boid", "project.yaml"), []byte("invalid: ["), 0644); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	applyThrough(t, d.Conn, "0061_workspace_receivers")
	if _, err := d.Conn.Exec(`INSERT INTO projects(id,work_dir) VALUES ('p',?)`, dir); err != nil {
		t.Fatal(err)
	}
	if err := Apply(d.Conn); err == nil {
		t.Fatal("malformed identity silently migrated")
	}
	exists, err := columnExists(d.Conn, "projects", "name")
	if err != nil || exists {
		t.Fatalf("failed migration did not roll back: %v %v", exists, err)
	}
}
