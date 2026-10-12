package migrate

import (
	"bytes"
	"log/slog"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
)

func TestProjectUpstreamsMigrationRecoversLegacyURLsOnce(t *testing.T) {
	for _, version := range []string{"0027_add_jobs_display_name", "0063_register_extra_repositories"} {
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
	applyThrough(t, d.Conn, "0063_register_extra_repositories")
	mirror := filepath.Join(t.TempDir(), "secret.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", mirror).CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
	if err := exec.Command("git", "--git-dir="+mirror, "config", "remote.origin.url", "https://user:SECRET@github.com/org/repo.git").Run(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Conn.Exec(`INSERT INTO projects(id,name,work_dir,upstream_url) VALUES ('missing','missing','/missing/mirror',NULL),('empty','empty','',NULL),('secret','secret',?,NULL),('existing','existing',?,'https://github.com/org/keep.git')`, mirror, mirror); err != nil {
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
	if err := d.Conn.QueryRow(`SELECT count(*) FROM projects WHERE upstream_url IS NULL`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("null count=%d err=%v", count, err)
	}
	var upstream string
	if err := d.Conn.QueryRow(`SELECT upstream_url FROM projects WHERE id='existing'`).Scan(&upstream); err != nil || upstream != "https://github.com/org/keep.git" {
		t.Fatalf("existing=%q err=%v", upstream, err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || strings.Contains(logs.String(), "SECRET") {
		t.Fatalf("unsafe or missing warning: %s", logs.String())
	}
}
