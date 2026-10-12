package migrate

import (
	"bytes"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestProjectNamesMigrationFallsBackForUnavailableLegacyIdentity(t *testing.T) {
	for _, scenario := range []string{"renamed-default-branch", "empty-upstream", "malformed", "unreadable", "whitespace"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			work := filepath.Join(root, "my-service.git")
			git := func(args ...string) {
				t.Helper()
				if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, out)
				}
			}
			switch scenario {
			case "renamed-default-branch", "empty-upstream":
				upstream := filepath.Join(root, "upstream")
				git("init", "-q", "-b", "master", upstream)
				if scenario == "renamed-default-branch" {
					git("-C", upstream, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "initial")
				}
				git("clone", "-q", "--mirror", upstream, work)
				if scenario == "renamed-default-branch" {
					git("-C", upstream, "branch", "-m", "main")
					git("--git-dir="+work, "fetch", "--prune")
				}
			default:
				if err := os.MkdirAll(filepath.Join(work, ".boid"), 0755); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(work, ".boid", "project.yaml")
				if scenario == "unreadable" {
					// A directory at the file path fails even when tests run as root.
					if err := os.Mkdir(file, 0755); err != nil {
						t.Fatal(err)
					}
				} else {
					content := "invalid: ["
					if scenario == "whitespace" {
						content = "name: '   '\n"
					}
					if err := os.WriteFile(file, []byte(content), 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			var logs bytes.Buffer
			previous := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(previous)
			d, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			applyThrough(t, d.Conn, "0061_workspace_receivers")
			if _, err := d.Conn.Exec(`INSERT INTO projects(id,work_dir) VALUES ('p',?)`, work); err != nil {
				t.Fatal(err)
			}
			if err := Apply(d.Conn); err != nil {
				t.Fatalf("upgrade failed: %v", err)
			}
			var name string
			if err := d.Conn.QueryRow(`SELECT name FROM projects WHERE id='p'`).Scan(&name); err != nil || name != "my-service" {
				t.Fatalf("name=%q err=%v", name, err)
			}
			if scenario != "whitespace" && !strings.Contains(logs.String(), "level=WARN") {
				t.Fatalf("missing warning: %s", logs.String())
			}
			if scenario == "renamed-default-branch" || scenario == "empty-upstream" {
				if !strings.Contains(logs.String(), "fatal:") {
					t.Fatalf("git stderr missing from warning: %s", logs.String())
				}
			}
			if err := Apply(d.Conn); err != nil {
				t.Fatalf("restart failed: %v", err)
			}
		})
	}
}
