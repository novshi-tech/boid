package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/testutil"
	"github.com/spf13/cobra"
)

// PR8 of docs/plans/workspace-home-volume-persistence.md (論点 f), CLI half:
// `boid workspace import-home <slug> [--from <dir>]`.
//
// The command's job is small and every part of it is a safety property (D4):
// resolve a default source that is right on the machine the legacy homes are
// actually on, refuse anything that is not a workspace home before a single
// byte is sent, always confirm before the daemon destroys a volume, and never
// write to the source.

func newImportHomeTestCmd(t *testing.T, ts *testutil.TestServer, out *bytes.Buffer) *cobra.Command {
	t.Helper()
	c := workspaceImportHomeCmd
	c.SetOut(out)
	c.SetErr(out)
	c.SetContext(client.WithClient(context.Background(), ts.Client))
	return c
}

// TestWorkspaceImportHome_DefaultSourceIsTheLegacyHomesDir pins D5's
// `--from` default. It has to resolve to the pre-PR6 layout on the HOST the
// CLI runs on, which is the one place those directories exist —
// dispatcher.WorkspaceHomesDir's own doc comment names this command as its
// remaining reader.
func TestWorkspaceImportHome_DefaultSourceIsTheLegacyHomesDir(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	got, err := defaultWorkspaceHomeSource("team-a")
	if err != nil {
		t.Fatalf("defaultWorkspaceHomeSource: %v", err)
	}
	want := filepath.Join(dataHome, "boid", "homes", "team-a")
	if got != want {
		t.Errorf("default --from = %q, want %q", got, want)
	}

	// And it agrees with the function the daemon side exports for the same
	// root, rather than re-deriving the layout independently.
	root, err := dispatcher.WorkspaceHomesDir("")
	if err != nil {
		t.Fatalf("WorkspaceHomesDir: %v", err)
	}
	if got != filepath.Join(root, "team-a") {
		t.Errorf("default --from = %q, want <WorkspaceHomesDir>/team-a = %q", got, filepath.Join(root, "team-a"))
	}
}

// TestWorkspaceImportHome_RefusesAMissingOrNonDirectorySource: the daemon
// destroys the home volume before it reads the archive, so a typo'd --from
// must be caught on this side of the wire.
func TestWorkspaceImportHome_RefusesAMissingOrNonDirectorySource(t *testing.T) {
	ts := testutil.NewTestServer(t)
	testutil.SeedWorkspace(t, ts, "team-a")

	for name, source := range map[string]string{
		"missing":      filepath.Join(t.TempDir(), "nope"),
		"regular file": writeTempFile(t),
	} {
		out := new(bytes.Buffer)
		cmd := newImportHomeTestCmd(t, ts, out)
		workspaceImportHomeFrom = source
		workspaceImportHomeYes = true
		t.Cleanup(func() { workspaceImportHomeFrom, workspaceImportHomeYes = "", false })

		err := runWorkspaceImportHome(cmd, []string{"team-a"})
		if err == nil {
			t.Errorf("%s: import-home accepted %q as a workspace home", name, source)
		}
	}
}

// TestWorkspaceImportHome_FollowsASymlinkedSource is codex round 3's Blocker 2
// at the surface an operator actually types.
//
// `--from` pointing at a symlink to the real home is ordinary — that is how a
// home that lives on another disk gets referred to — and every pre-flight in
// this command used os.Stat, which follows the link and says "directory, fine".
// filepath.WalkDir does not follow it, so the archive came out EMPTY, the daemon
// destroyed the workspace's HOME volume before reading it, extracted nothing, and
// answered 200. The CLI then printed "imported" over a home that had just been
// erased.
//
// Asserted through --dry-run because that is the mode whose whole job is to
// report what a real run would send: if the walk does not descend, the numbers
// here are zero.
func TestWorkspaceImportHome_FollowsASymlinkedSource(t *testing.T) {
	ts := testutil.NewTestServer(t)
	testutil.SeedWorkspace(t, ts, "team-a")

	real := t.TempDir()
	writeAt(t, filepath.Join(real, ".claude", ".credentials.json"), "token", 0o600)
	writeAt(t, filepath.Join(real, "bin", "tool"), "#!/bin/sh\n", 0o755)
	link := filepath.Join(t.TempDir(), "team-a")
	if err := os.Symlink(real, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	out := new(bytes.Buffer)
	cmd := newImportHomeTestCmd(t, ts, out)
	workspaceImportHomeFrom = link
	workspaceImportHomeDryRun = true
	t.Cleanup(func() { workspaceImportHomeFrom, workspaceImportHomeDryRun = "", false })

	if err := runWorkspaceImportHome(cmd, []string{"team-a"}); err != nil {
		t.Fatalf("runWorkspaceImportHome: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "2 files") {
		t.Fatalf("dry-run of a symlinked --from reported %q: the walk did not follow the link, so a real run would have "+
			"streamed an EMPTY archive into the volume the daemon had already destroyed", text)
	}
	// The operator has to be able to see WHERE the bytes are really coming from:
	// the prompt right after this is the last thing standing between them and an
	// irreversible replacement.
	if !strings.Contains(text, real) {
		t.Errorf("dry-run output %q never names the directory the symlink resolves to (%s)", text, real)
	}
}

// TestWorkspaceImportHome_RefusesASourceWithNothingToMigrate is the defence that
// does not depend on having predicted the cause.
//
// The symlinked --from was one way to send an empty archive; a --from with a
// typo, a backup that was never populated, an unmounted mount point are others.
// All of them end the same way, and that ending is uniquely bad: the daemon
// removes the HOME volume before it reads the body, so "nothing to migrate"
// becomes "the home is now empty" with a 200 and a cheerful CLI message.
//
// So it is refused here, before the prompt and before a byte is sent.
func TestWorkspaceImportHome_RefusesASourceWithNothingToMigrate(t *testing.T) {
	ts := testutil.NewTestServer(t)
	testutil.SeedWorkspace(t, ts, "team-a")

	restore := workspaceImportHomeConfirmPrompt
	t.Cleanup(func() { workspaceImportHomeConfirmPrompt = restore })
	var asked int
	workspaceImportHomeConfirmPrompt = func(io.Reader) (bool, error) { asked++; return true, nil }

	for name, dryRun := range map[string]bool{"real run": false, "dry run": true} {
		t.Run(name, func(t *testing.T) {
			out := new(bytes.Buffer)
			cmd := newImportHomeTestCmd(t, ts, out)
			workspaceImportHomeFrom = t.TempDir() // exists, is a directory, is empty
			workspaceImportHomeDryRun = dryRun
			t.Cleanup(func() { workspaceImportHomeFrom, workspaceImportHomeDryRun = "", false })

			err := runWorkspaceImportHome(cmd, []string{"team-a"})
			if err == nil {
				t.Fatal("import-home accepted a source with nothing in it: the daemon would destroy the workspace's HOME " +
					"volume and replace it with an empty one")
			}
			if !strings.Contains(err.Error(), workspaceImportHomeFrom) {
				t.Errorf("error %q does not name the directory that turned out to be empty", err)
			}
			if !strings.Contains(err.Error(), "--from") {
				t.Errorf("error %q does not point at the flag the operator has to fix", err)
			}
		})
	}
	if asked != 0 {
		t.Errorf("the confirmation prompt fired %d times for a source with nothing to migrate; the refusal has to land "+
			"before an operator is asked to approve anything", asked)
	}
}

// TestWorkspaceImportHome_DryRunSendsNothing is the "見せるだけ" mode: an
// operator about to replace a 4.3GB home should be able to see what would move
// without moving it.
func TestWorkspaceImportHome_DryRunSendsNothing(t *testing.T) {
	ts := testutil.NewTestServer(t)
	testutil.SeedWorkspace(t, ts, "team-a")

	src := t.TempDir()
	writeAt(t, filepath.Join(src, ".claude", ".credentials.json"), "token", 0o600)
	writeAt(t, filepath.Join(src, "bin", "tool"), "#!/bin/sh\n", 0o755)

	out := new(bytes.Buffer)
	cmd := newImportHomeTestCmd(t, ts, out)
	workspaceImportHomeFrom = src
	workspaceImportHomeDryRun = true
	t.Cleanup(func() { workspaceImportHomeFrom, workspaceImportHomeDryRun = "", false })

	if err := runWorkspaceImportHome(cmd, []string{"team-a"}); err != nil {
		t.Fatalf("runWorkspaceImportHome: %v", err)
	}
	text := out.String()
	for _, want := range []string{src, "2 files", "dry-run"} {
		if !strings.Contains(text, want) {
			t.Errorf("dry-run output %q does not mention %q", text, want)
		}
	}
	// The volume the daemon would replace has to be named: it is what the
	// operator checks against `boid workspace show` before committing.
	if !strings.Contains(text, "team-a") {
		t.Errorf("dry-run output %q does not name the workspace", text)
	}
}

// TestWorkspaceImportHome_PromptsBeforeDestroying is D4's 「既定は対話確認」.
// The prompt is not decoration: answering the request destroys the workspace's
// current HOME volume, and there is no undo.
func TestWorkspaceImportHome_PromptsBeforeDestroying(t *testing.T) {
	ts := testutil.NewTestServer(t)
	testutil.SeedWorkspace(t, ts, "team-a")

	src := t.TempDir()
	writeAt(t, filepath.Join(src, "a"), "x", 0o644)

	restore := workspaceImportHomeConfirmPrompt
	t.Cleanup(func() { workspaceImportHomeConfirmPrompt = restore })
	var asked int
	workspaceImportHomeConfirmPrompt = func(io.Reader) (bool, error) {
		asked++
		return false, nil // "no"
	}

	out := new(bytes.Buffer)
	cmd := newImportHomeTestCmd(t, ts, out)
	workspaceImportHomeFrom = src
	t.Cleanup(func() { workspaceImportHomeFrom = "" })

	if err := runWorkspaceImportHome(cmd, []string{"team-a"}); err != nil {
		t.Fatalf("runWorkspaceImportHome: %v", err)
	}
	if asked != 1 {
		t.Errorf("the confirmation prompt fired %d times, want 1", asked)
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Errorf("output %q does not say the migration was abandoned", out.String())
	}
}

// TestWorkspaceImportHome_ReachesTheDaemon is the CLI -> HTTP -> daemon seam,
// run against a real in-process daemon.
//
// The daemon it talks to has no reachable engine (testutil.NewTestServer points
// DOCKER_HOST at a socket that does not exist — see testutil/homeenv), so the
// migration necessarily FAILS. That is the assertion: the failure has to come
// from the engine call at the very end of the chain, which proves the request
// crossed the CLI's flag parsing, its tar writer, the HTTP client, chi's route,
// the workspace-row precondition, WorkspaceHandler.HomeImporter and the
// Runner's own volume replacement. A 404, a 501 or a 400 would each mean the
// chain broke earlier.
func TestWorkspaceImportHome_ReachesTheDaemon(t *testing.T) {
	ts := testutil.NewTestServer(t)
	testutil.SeedWorkspace(t, ts, "team-a")

	src := t.TempDir()
	writeAt(t, filepath.Join(src, ".claude", ".credentials.json"), "token", 0o600)

	out := new(bytes.Buffer)
	cmd := newImportHomeTestCmd(t, ts, out)
	workspaceImportHomeFrom = src
	workspaceImportHomeYes = true
	t.Cleanup(func() { workspaceImportHomeFrom, workspaceImportHomeYes = "", false })

	err := runWorkspaceImportHome(cmd, []string{"team-a"})
	if err == nil {
		t.Fatal("the migration succeeded against a daemon with no reachable engine")
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "not found"):
		t.Fatalf("the daemon 404'd: the request never got past the workspace-row precondition: %v", err)
	case strings.Contains(msg, "cannot import a workspace home"):
		t.Fatalf("the daemon 501'd: WorkspaceHandler.HomeImporter was never wired: %v", err)
	case strings.Contains(msg, "Content-Type"):
		t.Fatalf("the CLI sent the wrong media type: %v", err)
	}
	if !strings.Contains(msg, "home volume") && !strings.Contains(msg, "docker") && !strings.Contains(msg, "connect") {
		t.Errorf("error %q does not look like it came from the daemon's engine call at the end of the chain", msg)
	}

	// D4's first rule, checked on the real path rather than only in the tar
	// writer's own unit test: the source is READ.
	data, rerr := os.ReadFile(filepath.Join(src, ".claude", ".credentials.json"))
	if rerr != nil || string(data) != "token" {
		t.Errorf("the source changed: %q (err %v)", data, rerr)
	}
}

// TestWorkspaceImportHome_RejectsAnInvalidSlug keeps a path-shaped argument
// from ever becoming part of a volume name.
func TestWorkspaceImportHome_RejectsAnInvalidSlug(t *testing.T) {
	out := new(bytes.Buffer)
	c := workspaceImportHomeCmd
	c.SetOut(out)
	c.SetErr(out)
	if err := runWorkspaceImportHome(c, []string{"../../etc"}); err == nil {
		t.Fatal("an invalid slug was accepted")
	}
}

func writeTempFile(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func writeAt(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatalf("chmod: %v", err)
	}
}

// TestWorkspaceHomeImportFailure_PrefersTheServersAnswer pins the precedence
// workspaceHomeImportFailure exists for.
//
// The case that matters is the first one: a workspace with a running job is
// refused at the START of the request, so the 409 lands while the CLI is still
// uploading and the resulting closed pipe would otherwise be reported instead —
// telling the operator to look at their own filesystem for a problem that is
// somewhere else entirely.
func TestWorkspaceHomeImportFailure_PrefersTheServersAnswer(t *testing.T) {
	inUseBody := []byte(`{"error":"workspace home \"khi\": refusing to migrate while a job is running in it"}`)

	err := workspaceHomeImportFailure("/src", http.StatusConflict, inUseBody, nil, io.ErrClosedPipe)
	if err == nil {
		t.Fatal("a 409 produced no error")
	}
	if !strings.Contains(err.Error(), "refusing to migrate while a job is running") {
		t.Errorf("error = %q, want the daemon's own 409 explanation, not the closed pipe it caused", err)
	}
	if strings.Contains(err.Error(), "closed pipe") {
		t.Errorf("error = %q leaks the pipe close, which is this command's own doing", err)
	}
}

func TestWorkspaceHomeImportFailure_PrefersARealWalkFailure(t *testing.T) {
	walkErr := errors.New("open /src/.claude/x: permission denied")
	err := workspaceHomeImportFailure("/src", 0, nil, errors.New("request failed: body read"), walkErr)
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error = %v, want the walk failure that caused the transport error", err)
	}
}

func TestWorkspaceHomeImportFailure_NilOnSuccess(t *testing.T) {
	if err := workspaceHomeImportFailure("/src", http.StatusOK, []byte(`{}`), nil, nil); err != nil {
		t.Errorf("error = %v, want nil", err)
	}
	// A closed pipe alongside a 200 is the ordinary end of a streamed upload the
	// server finished reading; it is not a failure.
	if err := workspaceHomeImportFailure("/src", http.StatusOK, []byte(`{}`), nil, io.ErrClosedPipe); err != nil {
		t.Errorf("error = %v, want nil", err)
	}
}

// TestFormatWorkspaceHomeImportResult_WarnsWhenTheMigrationRecordSurvives is
// the CLI end of codex round 2's Major 2, as corrected by round 4's Blocker 1.
//
// The daemon writes an in-progress record before it destroys anything and
// deletes it on success. A record it could NOT delete used to mean the next
// daemon start would treat this finished migration as an interrupted one and
// discard the home it just imported. Since round 4 that is only true when the
// daemon could not even move the record into its "the home was rebuilt" phase —
// and the difference decides whether the operator has to act before the next
// `boid start` or can ignore the line entirely.
//
// Both messages therefore have to be distinguishable, and the severe one has to
// name the file. Printing the severe one for the cheap case is not a harmless
// over-warning: it sends the operator hunting for a file inside the daemon's
// state volume — which, under the compose deploy, they cannot even reach without
// a shell in the container.
func TestFormatWorkspaceHomeImportResult_WarnsWhenTheMigrationRecordSurvives(t *testing.T) {
	const removeErr = "remove /home/boid/.local/share/boid/homes-meta/khi.migrating.json: permission denied"

	severe := formatWorkspaceHomeImportResult("khi", api.WorkspaceHomeImportResponse{
		Volume:                      "boid-ws-home-a1b2c3d4-khi",
		PreviousExisted:             true,
		MarkerRemoved:               true,
		MigrationRecordRemoveError:  removeErr,
		MigrationRecordDiscardsHome: true,
	})
	if !strings.Contains(severe, "khi.migrating.json") {
		t.Errorf("output %q does not name the file the operator has to delete", severe)
	}
	if !strings.Contains(severe, "NEXT daemon start") {
		t.Errorf("output %q does not say what happens if they do nothing (the next start discards this home)", severe)
	}

	// The cheap case: the record is stale bookkeeping and the home is safe. It is
	// still reported — leftover daemon state is worth saying — but it must not
	// claim the home is about to be destroyed, or the operator either panics or
	// (worse, and quickly) learns to ignore the line that means they should.
	mild := formatWorkspaceHomeImportResult("khi", api.WorkspaceHomeImportResponse{
		Volume:                     "boid-ws-home-a1b2c3d4-khi",
		PreviousExisted:            true,
		MarkerRemoved:              true,
		MigrationRecordRemoveError: removeErr,
	})
	if !strings.Contains(mild, "migration record") {
		t.Errorf("output %q says nothing about the leftover record at all", mild)
	}
	if strings.Contains(mild, "NEXT daemon start") || strings.Contains(mild, "discard this home") {
		t.Errorf("output %q threatens the operator's home for a record that no longer authorizes destroying it: the "+
			"migration closed the destruction window before the unlink failed, so the next start only deletes the file", mild)
	}

	// And it stays quiet on the ordinary path: a warning every operator learns to
	// ignore is not a warning.
	clean := formatWorkspaceHomeImportResult("khi", api.WorkspaceHomeImportResponse{
		Volume: "boid-ws-home-a1b2c3d4-khi", PreviousExisted: true, MarkerRemoved: true,
	})
	if strings.Contains(clean, "migration record") {
		t.Errorf("a successful migration printed a record warning: %q", clean)
	}
}
