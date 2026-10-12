package migrate

import (
	"bytes"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
)

func TestProjectUpstreamsMigrationRecoversLegacyURLsOnce(t *testing.T) {
	for _, version := range []string{"0027_add_jobs_display_name", "0062_persist_project_names"} {
		t.Run(version, func(t *testing.T) {
			d, err := db.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			applyThrough(t, d.Conn, version)
			mirror := filepath.Join(t.TempDir(), "legacy.git")
			for _, args := range [][]string{{"init", "--bare", "-q", mirror}, {"--git-dir=" + mirror, "config", "remote.origin.url", "git@github.com:org/legacy.git"}} {
				if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, out)
				}
			}
			if _, err := d.Conn.Exec(`INSERT INTO projects(id,work_dir) VALUES ('legacy',?)`, mirror); err != nil {
				t.Fatal(err)
			}
			if err := Apply(d.Conn); err != nil {
				t.Fatal(err)
			}
			var upstream string
			if err := d.Conn.QueryRow(`SELECT upstream_url FROM projects WHERE id='legacy'`).Scan(&upstream); err != nil || upstream != "https://github.com/org/legacy.git" {
				t.Fatalf("upstream=%q err=%v", upstream, err)
			}
			if err := exec.Command("git", "--git-dir="+mirror, "config", "remote.origin.url", "https://github.com/org/changed.git").Run(); err != nil {
				t.Fatal(err)
			}
			if err := Apply(d.Conn); err != nil {
				t.Fatal(err)
			}
			if err := d.Conn.QueryRow(`SELECT upstream_url FROM projects WHERE id='legacy'`).Scan(&upstream); err != nil || upstream != "https://github.com/org/legacy.git" {
				t.Fatalf("changed upstream=%q err=%v", upstream, err)
			}
		})
	}
}

func TestProjectUpstreamsMigrationWarnsAndSkipsUnavailableURLs(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	applyThrough(t, d.Conn, "0062_persist_project_names")
	mirror := filepath.Join(t.TempDir(), "secret.git")
	noOrigin := filepath.Join(t.TempDir(), "no-origin.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", noOrigin).CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
	if out, err := exec.Command("git", "init", "--bare", "-q", mirror).CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
	if err := exec.Command("git", "--git-dir="+mirror, "config", "remote.origin.url", "https://user:SECRET@github.com/org/repo.git").Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Conn.Exec(`INSERT INTO projects(id,name,work_dir,upstream_url) VALUES ('missing','missing','/missing/mirror',NULL),('empty','empty','',NULL),('secret','secret',?,NULL),('existing','existing',?,'https://github.com/org/keep.git'),('no-origin','no-origin',?,NULL)`, mirror, mirror, noOrigin); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	if err := Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := d.Conn.QueryRow(`SELECT count(*) FROM projects WHERE upstream_url IS NULL`).Scan(&count); err != nil || count != 4 {
		t.Fatalf("null count=%d err=%v", count, err)
	}
	var upstream string
	if err := d.Conn.QueryRow(`SELECT upstream_url FROM projects WHERE id='existing'`).Scan(&upstream); err != nil || upstream != "https://github.com/org/keep.git" {
		t.Fatalf("existing=%q err=%v", upstream, err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "SECRET") {
		t.Fatalf("unsafe or missing warning: %s", logs.String())
	}
	if !strings.Contains(logs.String(), "error=") || !strings.Contains(logs.String(), "exit status") {
		t.Fatalf("missing git failure detail: %s", logs.String())
	}
}

func TestProjectUpstreamsMigrationRunsBeforeExtraRepositories(t *testing.T) {
	for _, bare := range []bool{true, false} {
		for _, missingURL := range []any{nil, ""} {
			t.Run(fmt.Sprintf("bare=%t/url=%v", bare, missingURL), func(t *testing.T) {
				d, err := db.Open(":memory:")
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				applyThrough(t, d.Conn, "0062_persist_project_names")
				repo := filepath.Join(t.TempDir(), "legacy")
				args := []string{"init", "-q"}
				if bare {
					args = append(args, "--bare")
				}
				args = append(args, repo)
				if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
					t.Fatalf("git init: %v %s", err, out)
				}
				if out, err := exec.Command("git", "-C", repo, "config", "--local", "remote.origin.url", "git@github.com:org/lib.git").CombinedOutput(); err != nil {
					t.Fatalf("git config: %v %s", err, out)
				}
				if _, err := d.Conn.Exec(`INSERT INTO workspaces(slug,extra_repos) VALUES ('team','["https://github.com/org/lib.git"]'); INSERT INTO projects(id,name,work_dir,upstream_url,perm) VALUES ('legacy','Library',?,?,'push'); INSERT INTO project_workspaces(project_id,workspace_id) VALUES ('legacy','team')`, repo, missingURL); err != nil {
					t.Fatal(err)
				}
				if err := Apply(d.Conn); err != nil {
					t.Fatal(err)
				}
				var count int
				if err := d.Conn.QueryRow(`SELECT count(*) FROM projects`).Scan(&count); err != nil || count != 1 {
					t.Fatalf("project rows=%d err=%v; legacy registration must be reused", count, err)
				}
				var id, name, dir, upstream, perm, workspace, extra string
				if err := d.Conn.QueryRow(`SELECT p.id,p.name,p.work_dir,p.upstream_url,p.perm,pw.workspace_id,w.extra_repos FROM projects p JOIN project_workspaces pw ON pw.project_id=p.id JOIN workspaces w ON w.slug=pw.workspace_id`).Scan(&id, &name, &dir, &upstream, &perm, &workspace, &extra); err != nil {
					t.Fatal(err)
				}
				if id != "legacy" || name != "Library" || dir != repo || upstream != "https://github.com/org/lib.git" || perm != "push" || workspace != "team" || extra != "[]" {
					t.Fatalf("registration changed: %q %q %q %q %q %q %q", id, name, dir, upstream, perm, workspace, extra)
				}
				if err := Apply(d.Conn); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
