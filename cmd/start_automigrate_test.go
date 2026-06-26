package cmd

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/daemon"
)

// fakeStatus builds a small migration status used across the table.
func fakeStatus(dirs ...string) *daemon.StartupStatus {
	ps := make([]daemon.StartupMigrationProject, len(dirs))
	for i, d := range dirs {
		ps[i] = daemon.StartupMigrationProject{
			Dir:      d,
			ID:       fmt.Sprintf("id-%d", i+1),
			Messages: []string{fmt.Sprintf(`project.yaml: top-level "kits" is no longer supported.`)},
		}
	}
	return &daemon.StartupStatus{Kind: daemon.StartupKindMigration, Projects: ps}
}

// recordingMigrator captures the args of every MigrateProject call so the
// table tests can assert order, count, and short-circuiting.
type recordingMigrator struct {
	calls    []MigrateProjectOptions
	failOnIdx int  // 0 = never fail; 1 = fail on call #1; etc.
}

func (r *recordingMigrator) Run(opts MigrateProjectOptions) error {
	r.calls = append(r.calls, opts)
	if r.failOnIdx > 0 && len(r.calls) == r.failOnIdx {
		return errors.New("synthetic migrate failure")
	}
	return nil
}

func TestHandleMigrationFailure_NonTTYWithoutFlag(t *testing.T) {
	var out bytes.Buffer
	rec := &recordingMigrator{}
	status := fakeStatus("/a", "/b")

	retry, err := handleMigrationFailure(
		&out, strings.NewReader(""),
		status, "/tmp/boid.log",
		false, // autoYes
		false, // isTTY
		nil,   // prompt should NOT be called
		rec.Run,
	)
	if retry {
		t.Fatalf("retrySpawn=true, want false")
	}
	if err == nil || !strings.Contains(err.Error(), "re-run with --auto-migrate") {
		t.Fatalf("err = %v, want re-run hint", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("migrator called %d times; want 0", len(rec.calls))
	}
	s := out.String()
	if !strings.Contains(s, "/a") || !strings.Contains(s, "/b") {
		t.Fatalf("summary missing project dirs: %s", s)
	}
	if !strings.Contains(s, "--auto-migrate") {
		t.Fatalf("hint missing --auto-migrate suggestion: %s", s)
	}
	for _, p := range []string{"  boid project migrate /a --apply", "  boid project migrate /b --apply"} {
		if !strings.Contains(s, p) {
			t.Fatalf("missing manual remediation line %q in: %s", p, s)
		}
	}
}

func TestHandleMigrationFailure_TTYPromptDecline(t *testing.T) {
	var out bytes.Buffer
	rec := &recordingMigrator{}
	status := fakeStatus("/a")

	declinePrompt := func(_ io.Writer, _ io.Reader) (bool, error) { return false, nil }

	retry, err := handleMigrationFailure(
		&out, strings.NewReader("n\n"),
		status, "/tmp/boid.log",
		false, // autoYes
		true,  // isTTY
		declinePrompt,
		rec.Run,
	)
	if retry {
		t.Fatalf("retrySpawn=true, want false")
	}
	if err == nil || !strings.Contains(err.Error(), "declined by user") {
		t.Fatalf("err = %v, want decline error", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("migrator called %d times; want 0", len(rec.calls))
	}
}

func TestHandleMigrationFailure_TTYPromptAccept(t *testing.T) {
	var out bytes.Buffer
	rec := &recordingMigrator{}
	status := fakeStatus("/a", "/b", "/c")

	retry, err := handleMigrationFailure(
		&out, strings.NewReader("y\n"),
		status, "/tmp/boid.log",
		false, // autoYes
		true,  // isTTY
		acceptPrompt,
		rec.Run,
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !retry {
		t.Fatalf("retrySpawn=false, want true (all migrations succeeded)")
	}
	if len(rec.calls) != 3 {
		t.Fatalf("migrator called %d times; want 3", len(rec.calls))
	}
	for i, want := range []string{"/a", "/b", "/c"} {
		if rec.calls[i].Dir != want {
			t.Fatalf("call[%d].Dir = %q, want %q", i, rec.calls[i].Dir, want)
		}
		if !rec.calls[i].Apply {
			t.Fatalf("call[%d].Apply = false, want true", i)
		}
		if rec.calls[i].OnCollision != "refuse" {
			t.Fatalf("call[%d].OnCollision = %q, want refuse", i, rec.calls[i].OnCollision)
		}
	}
}

func TestHandleMigrationFailure_StopsOnFirstFailure(t *testing.T) {
	var out bytes.Buffer
	rec := &recordingMigrator{failOnIdx: 2} // 2nd call fails
	status := fakeStatus("/a", "/b", "/c")

	retry, err := handleMigrationFailure(
		&out, strings.NewReader("y\n"),
		status, "/tmp/boid.log",
		false, // autoYes
		true,  // isTTY
		acceptPrompt,
		rec.Run,
	)
	if retry {
		t.Fatalf("retrySpawn=true, want false (failure should abort)")
	}
	if err == nil {
		t.Fatalf("err = nil, want failure error")
	}
	if !strings.Contains(err.Error(), `/b`) {
		t.Fatalf("err = %v, want mention of /b", err)
	}
	if len(rec.calls) != 2 {
		t.Fatalf("migrator called %d times; want 2 (third should be skipped)", len(rec.calls))
	}
	if rec.calls[0].Dir != "/a" || rec.calls[1].Dir != "/b" {
		t.Fatalf("call order wrong: %+v", rec.calls)
	}
}

func TestHandleMigrationFailure_AutoYesSkipsPromptOnNonTTY(t *testing.T) {
	var out bytes.Buffer
	rec := &recordingMigrator{}
	status := fakeStatus("/x", "/y")

	forbidPrompt := func(_ io.Writer, _ io.Reader) (bool, error) {
		t.Fatalf("prompt must not be called when autoYes=true")
		return false, nil
	}

	retry, err := handleMigrationFailure(
		&out, strings.NewReader(""),
		status, "/tmp/boid.log",
		true,  // autoYes
		false, // isTTY (CI/script)
		forbidPrompt,
		rec.Run,
	)
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !retry {
		t.Fatalf("retrySpawn=false, want true")
	}
	if len(rec.calls) != 2 {
		t.Fatalf("migrator called %d times; want 2", len(rec.calls))
	}
	if !strings.Contains(out.String(), "--auto-migrate:") {
		t.Fatalf("missing --auto-migrate banner: %s", out.String())
	}
}

func TestHandleMigrationFailure_AutoYesOnTTYStillSkipsPrompt(t *testing.T) {
	var out bytes.Buffer
	rec := &recordingMigrator{}
	status := fakeStatus("/x")

	forbidPrompt := func(_ io.Writer, _ io.Reader) (bool, error) {
		t.Fatalf("prompt must not be called when autoYes=true")
		return false, nil
	}

	retry, err := handleMigrationFailure(
		&out, strings.NewReader(""),
		status, "/tmp/boid.log",
		true, // autoYes
		true, // isTTY
		forbidPrompt,
		rec.Run,
	)
	if err != nil || !retry || len(rec.calls) != 1 {
		t.Fatalf("got retry=%v err=%v calls=%d; want true, nil, 1", retry, err, len(rec.calls))
	}
}

// TestDefaultMigratePrompter exercises the default y/N parser end-to-end
// (no fakes) so the actual prompt path used in the parent select loop is
// covered.
func TestDefaultMigratePrompter(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"Y\n", true},
		{"yes\n", true},
		{"YES\n", true},
		{"n\n", false},
		{"\n", false},
		{"anything else\n", false},
	}
	for _, tc := range cases {
		t.Run(strings.TrimSpace(tc.in), func(t *testing.T) {
			var out bytes.Buffer
			got, err := defaultMigratePrompter(&out, strings.NewReader(tc.in))
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("got=%v want=%v in=%q", got, tc.want, tc.in)
			}
			if !strings.Contains(out.String(), "Proceed?") {
				t.Fatalf("prompt text missing: %q", out.String())
			}
		})
	}
}

// acceptPrompt is a test helper that always returns true. It matches the
// real autoMigratePrompter signature (io.Writer, io.Reader).
func acceptPrompt(_ io.Writer, _ io.Reader) (bool, error) {
	return true, nil
}
