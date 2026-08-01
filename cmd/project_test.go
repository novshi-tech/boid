package cmd

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestRenderProjectDetail_BasicFields(t *testing.T) {
	p := &projectspec.Project{
		ID:          "proj-abc",
		WorkspaceID: "ws-1",
		WorkDir:     "/home/user/repo",
		CreatedAt:   time.Unix(0, 0).UTC(),
		UpdatedAt:   time.Unix(0, 0).UTC(),
		Meta: projectspec.ProjectMeta{
			Name: "My Project",
		},
	}

	got := captureStdout(t, func() {
		renderProjectDetail(p)
	})

	checks := []string{
		"ID:", "proj-abc",
		"Name:", "My Project",
		"WorkDir:", "/home/user/repo",
		"WorkspaceID:", "ws-1",
		"CreatedAt:",
		"UpdatedAt:",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}
}

func TestRenderProjectDetail_UpstreamURL_Set(t *testing.T) {
	p := &projectspec.Project{
		ID:          "proj-abc",
		WorkDir:     "/home/user/repo",
		UpstreamURL: "https://github.com/owner/repo.git",
		Meta:        projectspec.ProjectMeta{Name: "My Project"},
	}

	got := captureStdout(t, func() {
		renderProjectDetail(p)
	})

	if !strings.Contains(got, "UpstreamURL: https://github.com/owner/repo.git") {
		t.Errorf("output missing captured UpstreamURL\n%s", got)
	}
}

func TestRenderProjectDetail_UpstreamURL_Empty(t *testing.T) {
	p := &projectspec.Project{
		ID:      "proj-abc",
		WorkDir: "/home/user/repo",
		Meta:    projectspec.ProjectMeta{Name: "My Project"},
	}

	got := captureStdout(t, func() {
		renderProjectDetail(p)
	})

	if !strings.Contains(got, "UpstreamURL: (none") {
		t.Errorf("output missing empty-UpstreamURL guidance\n%s", got)
	}
}

func TestRenderProjectDetail_MetaSections(t *testing.T) {
	p := &projectspec.Project{
		ID: "proj-meta",
		Meta: projectspec.ProjectMeta{
			Name: "Meta Test",
			TaskBehaviors: map[string]projectspec.TaskBehavior{
				"dev": {
					Hooks: []projectspec.Hook{
						{ID: "on-start", Requires: []string{"gh"}},
					},
				},
			},
			HostCommands: projectspec.HostCommands{"gh": {}},
			AdditionalBindings: []projectspec.BindMount{
				{Source: "/data", Mode: "ro"},
			},
			Env: map[string]string{
				"GITHUB_TOKEN": "secret",
				"FOO":          "bar",
			},
			SecretNamespace: "myns",
		},
	}

	got := captureStdout(t, func() {
		renderProjectDetail(p)
	})

	checks := []string{
		"TaskBehaviors:",
		"dev",
		"hook: on-start",
		"HostCommands:",
		"gh",
		"AdditionalBindings:",
		"/data",
		"ro",
		"Env:",
		"FOO",
		"GITHUB_TOKEN",
		"SecretNamespace:",
		"myns",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}
}

func TestRenderProjectBehaviors_AlphaOrder(t *testing.T) {
	p := &projectspec.Project{
		ID: "proj-beh",
		Meta: projectspec.ProjectMeta{
			TaskBehaviors: map[string]projectspec.TaskBehavior{
				"zzz": {},
				"aaa": {},
				"mmm": {},
			},
		},
	}

	got := captureStdout(t, func() {
		renderProjectBehaviors(p)
	})

	// キーがアルファベット順で出ること
	idxA := strings.Index(got, "aaa")
	idxM := strings.Index(got, "mmm")
	idxZ := strings.Index(got, "zzz")
	if idxA < 0 || idxM < 0 || idxZ < 0 {
		t.Fatalf("missing keys in output:\n%s", got)
	}
	if !(idxA < idxM && idxM < idxZ) {
		t.Errorf("behaviors not in alphabetical order (a=%d m=%d z=%d):\n%s", idxA, idxM, idxZ, got)
	}
}

func TestRenderProjectBehaviors_Fields(t *testing.T) {
	p := &projectspec.Project{
		ID: "proj-beh2",
		Meta: projectspec.ProjectMeta{
			TaskBehaviors: map[string]projectspec.TaskBehavior{
				"dev": {
					Traits: []string{"artifact", "worktree"},
				},
			},
		},
	}

	got := captureStdout(t, func() {
		renderProjectBehaviors(p)
	})

	checks := []string{
		"dev",
		"artifact",
	}
	for _, want := range checks {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n%s", want, got)
		}
	}
}

func TestRenderProjectBehaviors_Empty(t *testing.T) {
	p := &projectspec.Project{
		ID: "proj-empty",
		Meta: projectspec.ProjectMeta{
			TaskBehaviors: map[string]projectspec.TaskBehavior{},
		},
	}

	got := captureStdout(t, func() {
		renderProjectBehaviors(p)
	})

	if !strings.Contains(got, "no behaviors") {
		t.Errorf("expected 'no behaviors' message, got:\n%s", got)
	}
}

// TestProjectAddCmd_HasWorkspaceFlag verifies that the --workspace flag is
// registered on `boid project add`.
func TestProjectAddCmd_HasWorkspaceFlag(t *testing.T) {
	f := projectAddCmd.Flags().Lookup("workspace")
	if f == nil {
		t.Fatal("--workspace flag not registered on project add")
	}
	if f.DefValue != "" {
		t.Errorf("expected empty default for --workspace, got %q", f.DefValue)
	}
}

// TestProjectInitSubCmd_HasWorkspaceFlag verifies that --workspace is
// registered on `boid project init`.
func TestProjectInitSubCmd_HasWorkspaceFlag(t *testing.T) {
	f := projectInitSubCmd.Flags().Lookup("workspace")
	if f == nil {
		t.Fatal("--workspace flag not registered on project init")
	}
	if f.DefValue != "" {
		t.Errorf("expected empty default for --workspace, got %q", f.DefValue)
	}
}

// TestProjectInitSubCmd_HasSkipAutostart pins Blocker (codex round-16
// review): `project init` used to require a live daemon (the now-removed
// POST /api/projects registration call), which is why cmd/root.go's
// PersistentPreRunE autostarts a bare-host daemon by default for scopeLocal
// commands lacking annotationSkipAutostart — but runProjectInit no longer
// talks to the daemon in any way (docs/plans/release-onboarding.md 穴
// 7/PR6), so that autostart is now unnecessary and could even block a user
// with BOID_NO_AUTOSTART=1 (or no daemon set up yet) from reaching the
// wizard at all.
func TestProjectInitSubCmd_HasSkipAutostart(t *testing.T) {
	if projectInitSubCmd.Annotations[annotationSkipAutostart] != "skip" {
		t.Error("projectInitSubCmd must have annotationSkipAutostart=skip")
	}
}

// withStdin temporarily replaces os.Stdin for the duration of the test.
// runProjectInit's Wizard reads from os.Stdin directly (not
// cmd.InOrStdin()), so exercising it end-to-end needs the process-global
// swapped out.
func withStdin(t *testing.T, r *os.File) {
	t.Helper()
	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = orig })
}

// devNullStdin opens /dev/null so the wizard's name prompt hits EOF
// immediately and falls back to the directory-basename default — the
// project name itself is irrelevant to what this file's project-init tests
// pin (the post-scaffold guidance), so no real interactive input is needed.
func devNullStdin(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

// TestProjectInit_NoRemote_GuidesGitInitAddPush pins 穴 7's fix
// (docs/plans/release-onboarding.md): `project init` must NOT attempt to
// register the host-local projectDir with the daemon anymore (the old
// POST /api/projects work_dir-based call always 400s under a compose
// daemon, since that directory doesn't exist inside the daemon's
// container). Instead it scaffolds and then prints the "push it, then
// register the URL" guidance the plan doc's 目標オンボーディングフロー spells
// out. No BOID_SOCKET is set at all here — if runProjectInit still tried to
// talk to a daemon, client.FromContext would either panic or dial a
// nonexistent socket and error, so a clean, guidance-only exit run without
// any daemon proves the network call is gone.
func TestProjectInit_NoRemote_GuidesGitInitAddPush(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, devNullStdin(t))

	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := runProjectInit(cmd, []string{dir}); err != nil {
		t.Fatalf("runProjectInit: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "git init") {
		t.Errorf("expected guidance to mention git init, got:\n%s", got)
	}
	if !strings.Contains(got, "git push") {
		t.Errorf("expected guidance to mention git push, got:\n%s", got)
	}
	if !strings.Contains(got, "boid project add '<git-url>'") {
		t.Errorf("expected guidance to point at `boid project add '<git-url>'`, got:\n%s", got)
	}
	// Default-branch-aware push (codex round-7/round-8/round-15 review):
	// the daemon reads .boid/project.yaml off the remote's DEFAULT
	// branch specifically, which the just-checked-out local branch is
	// not guaranteed to be. The guidance resolves the remote's actual
	// default branch via `git ls-remote --symref` and pushes there
	// directly, falling back to the current branch's own name only when
	// the remote has no resolvable default yet (a genuinely fresh, empty
	// repository).
	if !strings.Contains(got, "git ls-remote --symref") {
		t.Errorf("expected guidance to resolve the remote's default branch via git ls-remote --symref, got:\n%s", got)
	}
	// "Your actual code, not just the scaffold" caveat (codex round-13
	// review): the chain only ever commits/pushes .boid/project.yaml
	// (deliberately, to avoid sweeping in unrelated staged changes —
	// round-4/round-9 review) — for a codebase that already exists but
	// isn't pushed yet, that alone would register a project with no
	// real source for an agent to work on. Must be called out
	// explicitly, not left implicit.
	if !strings.Contains(got, "actual source code") {
		t.Errorf("expected guidance to explicitly call out committing/pushing the project's actual source code (not just the scaffold), got:\n%s", got)
	}

	if _, err := os.Stat(filepath.Join(dir, ".boid", "project.yaml")); err != nil {
		t.Errorf("expected scaffold to still be written: %v", err)
	}
}

// TestProjectInit_WithWorkspaceFlag_IncludesItInGuidance pins that a
// --workspace value passed to `project init` flows through into the printed
// `boid project add` example rather than being silently dropped now that
// init no longer performs the workspace assignment itself.
func TestProjectInit_WithWorkspaceFlag_IncludesItInGuidance(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, devNullStdin(t))
	projectInitWorkspace = "my-ws"
	t.Cleanup(func() { projectInitWorkspace = "" })

	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := runProjectInit(cmd, []string{dir}); err != nil {
		t.Fatalf("runProjectInit: %v", err)
	}

	if !strings.Contains(out.String(), "--workspace=my-ws") {
		t.Errorf("expected guidance to reference --workspace=my-ws, got:\n%s", out.String())
	}
}

// TestProjectInit_NoWorkspaceFlag_DefaultsToDefaultWorkspace pins the fix
// for codex round-1 Major review of this PR: with --workspace omitted, the
// printed `boid project add` example used to show the literal placeholder
// "--workspace=<workspace>" — not runnable as-is, contradicting the help
// text's "exact next commands" promise (--workspace is a required flag on
// `project add`). It must default to "--workspace=default", matching the
// 目標オンボーディングフロー step 5 example in docs/plans/release-onboarding.md.
func TestProjectInit_NoWorkspaceFlag_DefaultsToDefaultWorkspace(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, devNullStdin(t))

	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := runProjectInit(cmd, []string{dir}); err != nil {
		t.Fatalf("runProjectInit: %v", err)
	}

	if !strings.Contains(out.String(), "--workspace=default") {
		t.Errorf("expected guidance to default to --workspace=default, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "<workspace>") {
		t.Errorf("expected no unfilled <workspace> placeholder, got:\n%s", out.String())
	}
}

// TestProjectInit_DirArg_GitCommandsTargetProjectDir pins Blocker 1 (codex
// round-1 review of this PR): when [dir] is a path other than ".", every
// printed git command must explicitly target that directory (`cd
// <projectDir> && ...`) rather than relying on the invoking shell's own
// cwd — otherwise pasting the guidance verbatim silently git-inits/commits
// whatever directory the terminal happened to be in, not the freshly
// scaffolded one.
func TestProjectInit_DirArg_GitCommandsTargetProjectDir(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "my-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	withStdin(t, devNullStdin(t))

	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := runProjectInit(cmd, []string{dir}); err != nil {
		t.Fatalf("runProjectInit: %v", err)
	}

	got := out.String()
	// The whole chain must be ONE cd-prefixed, brace-grouped line (codex
	// round-3 and round-5 review, Blocker follow-ups): a `cd` on its own
	// printed line only carries over to a LATER printed line if the user
	// happens to paste both into the same still-open shell — copied
	// separately, re-run individually, or run from a fresh terminal, a
	// bare `git remote add origin`/`git push` on its own line would
	// silently target the wrong (or no) directory. `;` (not `&&`) between
	// the inner steps matters too: this exact guidance is documented as
	// "safe to run even if some of this is already done", and an idempotent
	// rerun's `git commit` (nothing new to commit) must not stop `git
	// remote add`/`git push` from still running.
	wantChain := "cd '" + dir + "' && { git init && git add .boid/project.yaml && (git diff --cached --quiet -- .boid/project.yaml || git commit -m 'add boid project scaffold' -- .boid/project.yaml) && git push '<git-url>' HEAD && { DEFAULT_REF=$(git ls-remote --symref '<git-url>' HEAD 2>/dev/null | awk '$1==\"ref:\"{print $2}'); CURRENT_REF=\"refs/heads/$(git symbolic-ref --short HEAD)\"; if [ -n \"$DEFAULT_REF\" ] && [ \"$DEFAULT_REF\" != \"$CURRENT_REF\" ]; then echo \"WARNING: pushed to $CURRENT_REF, but this remote's default branch is $DEFAULT_REF -- merge/PR it there before running step 3, or the daemon will not see .boid/project.yaml\" >&2; fi; }; }"
	if !strings.Contains(got, wantChain) {
		t.Errorf("expected guidance to contain the single cd-prefixed chain %q, got:\n%s", wantChain, got)
	}
}

// TestProjectInit_PrintedChain_OnlyCommitsProjectYAML executes the ACTUAL
// printed guidance chain (not just checking its text) against a fixture
// repo that already has an unrelated file under .boid/ (this project's own
// repo has exactly this shape: .boid/run-e2e-scenario.sh sits alongside
// .boid/project.yaml) — pinning Major (codex round-9 review): a bare
// `.boid` pathspec sweeps in every OTHER file already under .boid too, not
// just the one project.yaml this command wrote. Confirms the new commit
// touches only .boid/project.yaml.
func TestProjectInit_PrintedChain_OnlyCommitsProjectYAML(t *testing.T) {
	dir := t.TempDir()
	runGitTestCmd(t, dir, "init", "-q", "-b", "main")
	runGitTestCmd(t, dir, "config", "user.email", "test@example.com")
	runGitTestCmd(t, dir, "config", "user.name", "Test")
	if err := os.MkdirAll(filepath.Join(dir, ".boid"), 0o755); err != nil {
		t.Fatal(err)
	}
	otherBoidFile := filepath.Join(dir, ".boid", "run-e2e-scenario.sh")
	if err := os.WriteFile(otherBoidFile, []byte("#!/bin/sh\necho pre-existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTestCmd(t, dir, "add", ".boid/run-e2e-scenario.sh")
	runGitTestCmd(t, dir, "commit", "-q", "-m", "pre-existing .boid content")
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTestCmd(t, t.TempDir(), "init", "-q", "--bare", remote)

	withStdin(t, devNullStdin(t))
	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := runProjectInit(cmd, []string{dir}); err != nil {
		t.Fatalf("runProjectInit: %v", err)
	}

	// Extract the printed one-liner and actually run it, substituting the
	// real remote for the '<git-url>' placeholder — exercising the exact
	// text a user would paste, not a hand-written equivalent.
	got := out.String()
	start := strings.Index(got, "{ git init &&")
	end := strings.LastIndex(got[start:], "; }") + start + len("; }")
	if start < 0 || end <= start {
		t.Fatalf("could not locate the printed chain in guidance:\n%s", got)
	}
	chain := strings.ReplaceAll(got[start:end], "'<git-url>'", "'"+remote+"'")

	runBashCmd(t, dir, chain)

	newCommitFiles := runGitTestCmdOutput(t, dir, "show", "--stat", "--pretty=format:", "HEAD")
	if !strings.Contains(newCommitFiles, "project.yaml") {
		t.Errorf("expected HEAD to include project.yaml, got:\n%s", newCommitFiles)
	}
	if strings.Contains(newCommitFiles, "run-e2e-scenario.sh") {
		t.Errorf("expected HEAD to NOT re-touch the pre-existing run-e2e-scenario.sh, got:\n%s", newCommitFiles)
	}
}

// runBashCmd runs script via `bash -c` with dir as cwd, failing the test on
// a non-zero exit (with combined output attached for diagnosis).
func runBashCmd(t *testing.T, dir, script string) {
	t.Helper()
	c := exec.Command("bash", "-c", script)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("bash -c %q: %v\n%s", script, err, out)
	}
}

// runGitTestCmdOutput runs a git command and returns its stdout, failing
// the test on a non-zero exit.
func runGitTestCmdOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// TestProjectInit_PrintedChain_DoesNotTouchExistingRemoteConfig executes
// the ACTUAL printed guidance chain against a fixture repo whose `origin`
// deliberately points somewhere UNRELATED to the URL being pushed to, with
// THREE pre-existing `remote.origin.pushurl` overrides on top — pinning
// Major (codex round-12 review): two earlier revisions of this guidance
// tried progressively more clever things with `origin` itself (detecting
// and printing it back, then unconditionally repointing it and normalizing
// its pushurl overrides) and kept finding new bugs doing so (shell
// injection, parent-repo misattribution, a stale pushurl, then multiple
// stale pushurls) — the fix that actually ends the cycle is a one-shot
// `git push <url> HEAD` that never creates, reads, or modifies any named
// remote at all. This test confirms both halves of that: the scaffold
// lands in the intended URL, AND `origin`/its pushurls are left completely
// untouched (not "fixed" — never consulted in the first place).
func TestProjectInit_PrintedChain_DoesNotTouchExistingRemoteConfig(t *testing.T) {
	dir := t.TempDir()
	runGitTestCmd(t, dir, "init", "-q", "-b", "main")
	runGitTestCmd(t, dir, "config", "user.email", "test@example.com")
	runGitTestCmd(t, dir, "config", "user.name", "Test")
	runGitTestCmd(t, dir, "commit", "-q", "--allow-empty", "-m", "initial")

	unrelatedRemote := filepath.Join(t.TempDir(), "unrelated.git")
	stalePushRemote1 := filepath.Join(t.TempDir(), "stale1.git")
	stalePushRemote2 := filepath.Join(t.TempDir(), "stale2.git")
	realRemote := filepath.Join(t.TempDir(), "real.git")
	for _, r := range []string{unrelatedRemote, stalePushRemote1, stalePushRemote2, realRemote} {
		runGitTestCmd(t, t.TempDir(), "init", "-q", "--bare", r)
	}
	runGitTestCmd(t, dir, "remote", "add", "origin", unrelatedRemote)
	runGitTestCmd(t, dir, "config", "--local", "--add", "remote.origin.pushurl", stalePushRemote1)
	runGitTestCmd(t, dir, "config", "--local", "--add", "remote.origin.pushurl", stalePushRemote2)
	originURLBefore := runGitTestCmdOutput(t, dir, "config", "--local", "--get", "remote.origin.url")
	pushURLsBefore := runGitTestCmdOutput(t, dir, "config", "--local", "--get-all", "remote.origin.pushurl")

	withStdin(t, devNullStdin(t))
	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := runProjectInit(cmd, []string{dir}); err != nil {
		t.Fatalf("runProjectInit: %v", err)
	}

	got := out.String()
	start := strings.Index(got, "{ git init &&")
	end := strings.LastIndex(got[start:], "; }") + start + len("; }")
	if start < 0 || end <= start {
		t.Fatalf("could not locate the printed chain in guidance:\n%s", got)
	}
	chain := strings.ReplaceAll(got[start:end], "'<git-url>'", "'"+realRemote+"'")
	runBashCmd(t, dir, chain)

	if originURLAfter := runGitTestCmdOutput(t, dir, "config", "--local", "--get", "remote.origin.url"); originURLAfter != originURLBefore {
		t.Errorf("expected remote.origin.url to be untouched, was %q, now %q", originURLBefore, originURLAfter)
	}
	if pushURLsAfter := runGitTestCmdOutput(t, dir, "config", "--local", "--get-all", "remote.origin.pushurl"); pushURLsAfter != pushURLsBefore {
		t.Errorf("expected remote.origin.pushurl entries to be untouched, was:\n%s\nnow:\n%s", pushURLsBefore, pushURLsAfter)
	}

	realRemoteRefs, _ := exec.Command("git", "--git-dir="+realRemote, "show-ref").CombinedOutput()
	if !strings.Contains(string(realRemoteRefs), "refs/heads/main") {
		t.Errorf("expected the scaffold to have been pushed to the intended URL %s, show-ref output:\n%s", realRemote, realRemoteRefs)
	}
	for _, other := range []string{unrelatedRemote, stalePushRemote1, stalePushRemote2} {
		showRefOut, _ := exec.Command("git", "--git-dir="+other, "show-ref").CombinedOutput()
		if strings.Contains(string(showRefOut), "refs/heads/main") {
			t.Errorf("did not expect the scaffold to have been pushed to %s, but show-ref shows:\n%s", other, showRefOut)
		}
	}
}

// TestProjectInit_GuidanceIsIdempotentRegardlessOfExistingGitState pins the
// design this revision settled on after four rounds of review kept finding
// new correctness holes in an earlier "detect existing origin and print its
// URL back" version (unquoted shell injection, `git config`'s upward
// search misattributing a PARENT repo's origin, a bare `git commit`
// sweeping in unrelated staged changes, a file:// origin printed as if
// registerable, and finally a `git remote add` unconditionally failing
// once origin already exists): the SAME guidance text is printed
// regardless of whether projectDir has no repo yet, already has a repo,
// or already has `origin` configured — and every step in it is written to
// be a safe no-op if that step already happened. This test pins that
// property directly: running against an ALREADY-existing repo with an
// ALREADY-configured origin produces byte-identical guidance to a bare
// empty directory (aside from the `cd` path itself).
func TestProjectInit_GuidanceIsIdempotentRegardlessOfExistingGitState(t *testing.T) {
	freshDir := t.TempDir()
	existingDir := t.TempDir()
	runGitTestCmd(t, existingDir, "init", "-q", "-b", "main")
	runGitTestCmd(t, existingDir, "config", "user.email", "test@example.com")
	runGitTestCmd(t, existingDir, "config", "user.name", "Test")
	runGitTestCmd(t, existingDir, "commit", "-q", "--allow-empty", "-m", "initial")
	runGitTestCmd(t, existingDir, "remote", "add", "origin", "https://example.invalid/owner/repo.git")

	withStdin(t, devNullStdin(t))
	var freshOut bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&freshOut)
	cmd.SetContext(context.Background())
	if err := runProjectInit(cmd, []string{freshDir}); err != nil {
		t.Fatalf("runProjectInit (fresh): %v", err)
	}

	withStdin(t, devNullStdin(t))
	var existingOut bytes.Buffer
	cmd2 := projectInitSubCmd
	cmd2.SetOut(&existingOut)
	cmd2.SetContext(context.Background())
	if err := runProjectInit(cmd2, []string{existingDir}); err != nil {
		t.Fatalf("runProjectInit (existing): %v", err)
	}

	// Compare only the "Next steps:" section onward: the wizard's own
	// "Project name [<dir-basename>]:" prompt line and "Created <DIR>"
	// line legitimately differ (different t.TempDir() basenames), but
	// nothing about the printed git/registration guidance should.
	freshGuidance := freshOut.String()[strings.Index(freshOut.String(), "Next steps:"):]
	existingGuidance := existingOut.String()[strings.Index(existingOut.String(), "Next steps:"):]
	freshGuidance = strings.ReplaceAll(freshGuidance, freshDir, "<DIR>")
	existingGuidance = strings.ReplaceAll(existingGuidance, existingDir, "<DIR>")
	if freshGuidance != existingGuidance {
		t.Errorf("expected identical guidance modulo the cd path, got:\nfresh:\n%s\nexisting:\n%s", freshGuidance, existingGuidance)
	}
	// And that the pre-existing origin URL never leaks into the output at
	// all (it is deliberately never inspected or printed anymore).
	if strings.Contains(existingOut.String(), "example.invalid") {
		t.Errorf("did not expect the pre-existing origin URL to appear in guidance, got:\n%s", existingOut.String())
	}
}

// TestProjectInit_InvalidWorkspaceFlag_RejectsBeforeScaffolding pins Major
// (codex round-2 review of this PR): `boid project add` (the command
// project init's own guidance prints) requires a valid workspace slug
// (ValidWorkspaceSlug, runProjectAddGitURL) — an invalid --workspace value
// passed to `project init` must be rejected up front, before the scaffold
// is written, rather than succeeding and printing a `project add` command
// that is guaranteed to fail.
func TestProjectInit_InvalidWorkspaceFlag_RejectsBeforeScaffolding(t *testing.T) {
	dir := t.TempDir()
	withStdin(t, devNullStdin(t))
	projectInitWorkspace = "Team_A" // uppercase/underscore: not a valid slug
	t.Cleanup(func() { projectInitWorkspace = "" })

	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	err := runProjectInit(cmd, []string{dir})
	if err == nil {
		t.Fatal("expected an error for an invalid --workspace slug")
	}
	if !strings.Contains(err.Error(), "invalid --workspace value") {
		t.Errorf("expected an 'invalid --workspace value' error, got: %v", err)
	}

	if _, statErr := os.Stat(filepath.Join(dir, ".boid", "project.yaml")); statErr == nil {
		t.Error("expected no scaffold to be written when --workspace is rejected up front")
	}
}

// TestProjectInit_NestedInExistingRepo_RejectsBeforeScaffolding pins Major
// (codex round-15 review): scaffolding a NEW subdirectory that has no .git
// of its own but sits inside an ALREADY-existing, unrelated git repository
// must be rejected outright — a nested `git init` there would create a
// brand-new, unrelated repo whose commit/push history has nothing to do
// with the enclosing repo's, so the printed guidance's push would either
// fail (non-fast-forward against the enclosing repo's own remote) or, on a
// different remote, register a project containing only the scaffold with
// none of the enclosing repo's actual source. Caught before the scaffold
// is even written.
func TestProjectInit_NestedInExistingRepo_RejectsBeforeScaffolding(t *testing.T) {
	parent := t.TempDir()
	runGitTestCmd(t, parent, "init", "-q", "-b", "main")
	runGitTestCmd(t, parent, "config", "user.email", "test@example.com")
	runGitTestCmd(t, parent, "config", "user.name", "Test")
	runGitTestCmd(t, parent, "commit", "-q", "--allow-empty", "-m", "initial")

	dir := filepath.Join(parent, "my-project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	withStdin(t, devNullStdin(t))

	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	err := runProjectInit(cmd, []string{dir})
	if err == nil {
		t.Fatal("expected an error for a projectDir nested inside an existing repo")
	}
	if !strings.Contains(err.Error(), "nested inside an existing git repository") {
		t.Errorf("expected a 'nested inside an existing git repository' error, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".boid", "project.yaml")); statErr == nil {
		t.Error("expected no scaffold to be written when projectDir is rejected as nested")
	}
}

// TestProjectInit_NestedNotYetExistingSubdir_RejectsBeforeScaffolding pins
// Major (codex round-16 review): `boid project init ./brand-new-subdir`
// where `./brand-new-subdir` does not exist YET (only its parent, an
// already-existing repo, does) must also be rejected — a naive `git -C
// projectDir rev-parse --show-toplevel` simply errors when projectDir
// doesn't exist, indistinguishable from "no enclosing repo at all", which
// let this exact case slip through the round-15 fix uncaught (Wizard.Run
// then creates the directory itself via MkdirAll).
func TestProjectInit_NestedNotYetExistingSubdir_RejectsBeforeScaffolding(t *testing.T) {
	parent := t.TempDir()
	runGitTestCmd(t, parent, "init", "-q", "-b", "main")
	runGitTestCmd(t, parent, "config", "user.email", "test@example.com")
	runGitTestCmd(t, parent, "config", "user.name", "Test")
	runGitTestCmd(t, parent, "commit", "-q", "--allow-empty", "-m", "initial")

	// Deliberately NOT created — this is the whole point of the test.
	dir := filepath.Join(parent, "brand-new-subdir")

	withStdin(t, devNullStdin(t))
	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	err := runProjectInit(cmd, []string{dir})
	if err == nil {
		t.Fatal("expected an error for a not-yet-existing projectDir nested inside an existing repo")
	}
	if !strings.Contains(err.Error(), "nested inside an existing git repository") {
		t.Errorf("expected a 'nested inside an existing git repository' error, got: %v", err)
	}
	if _, statErr := os.Stat(dir); statErr == nil {
		t.Error("expected projectDir to NOT have been created at all when rejected as nested")
	}
}

// TestProjectInit_DirIsOwnRepoRoot_DoesNotReject pins the non-regression
// counterpart of the nested-repo rejection above: projectDir being a git
// repo's OWN root (not nested inside a DIFFERENT one) must still work
// exactly as before — this is the "user already ran git init themselves"
// starter scenario project init's own doc comment describes.
func TestProjectInit_DirIsOwnRepoRoot_DoesNotReject(t *testing.T) {
	dir := t.TempDir()
	runGitTestCmd(t, dir, "init", "-q", "-b", "main")
	runGitTestCmd(t, dir, "config", "user.email", "test@example.com")
	runGitTestCmd(t, dir, "config", "user.name", "Test")
	runGitTestCmd(t, dir, "commit", "-q", "--allow-empty", "-m", "initial")

	withStdin(t, devNullStdin(t))
	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := runProjectInit(cmd, []string{dir}); err != nil {
		t.Fatalf("runProjectInit: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".boid", "project.yaml")); statErr != nil {
		t.Errorf("expected scaffold to be written for a dir that IS its own repo root: %v", statErr)
	}
}

// TestProjectInit_PrintedChain_PushesToRemoteDefaultBranch executes the
// ACTUAL printed guidance chain against a fixture simulating a real forge
// repo: an established remote with commits already on "main" (its real
// default branch, confirmed via `git symbolic-ref HEAD` on the bare repo
// itself), cloned locally and then checked out onto an unrelated
// "feature-branch" before scaffolding — pinning Major (codex round-15
// review, correcting a factual error in an earlier revision's own comment
// claiming no portable client-side git command could determine a remote's
// default branch): `git ls-remote --symref` does exactly that, and the
// guidance uses it to push the scaffold commit onto the remote's ACTUAL
// default branch, not a new ref named after the local feature branch.
func TestProjectInit_PrintedChain_WarnsWithoutForcingOntoDefaultBranch(t *testing.T) {
	remote := filepath.Join(t.TempDir(), "remote.git")
	runGitTestCmd(t, t.TempDir(), "init", "-q", "--bare", remote)

	seed := t.TempDir()
	runGitTestCmd(t, seed, "init", "-q", "-b", "main")
	runGitTestCmd(t, seed, "config", "user.email", "test@example.com")
	runGitTestCmd(t, seed, "config", "user.name", "Test")
	runGitTestCmd(t, seed, "commit", "-q", "--allow-empty", "-m", "initial project commit")
	runGitTestCmd(t, seed, "push", "-q", remote, "main")
	runGitTestCmd(t, t.TempDir(), "--git-dir="+remote, "symbolic-ref", "HEAD", "refs/heads/main")

	clone := t.TempDir()
	runGitTestCmd(t, t.TempDir(), "clone", "-q", remote, clone)
	runGitTestCmd(t, clone, "config", "user.email", "test@example.com")
	runGitTestCmd(t, clone, "config", "user.name", "Test")
	runGitTestCmd(t, clone, "checkout", "-q", "-b", "feature-branch")

	withStdin(t, devNullStdin(t))
	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	if err := runProjectInit(cmd, []string{clone}); err != nil {
		t.Fatalf("runProjectInit: %v", err)
	}

	got := out.String()
	start := strings.Index(got, "{ git init &&")
	end := strings.LastIndex(got[start:], "; }") + start + len("; }")
	if start < 0 || end <= start {
		t.Fatalf("could not locate the printed chain in guidance:\n%s", got)
	}
	chain := strings.ReplaceAll(got[start:end], "'<git-url>'", "'"+remote+"'")
	c := exec.Command("bash", "-c", chain)
	c.Dir = clone
	chainOut, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("bash -c %q: %v\n%s", chain, err, chainOut)
	}

	// Blocker (codex round-16 review of an earlier revision): pushing
	// `HEAD:$DEFAULT_REF` unconditionally forced the feature branch's
	// commits directly onto the remote's protected default branch,
	// bypassing PR review (and failing outright against real branch
	// protection). The fix pushes the CURRENT branch under its own name
	// only, and merely WARNS — never redirects the push — when that
	// differs from the remote's actual default branch.
	if !strings.Contains(string(chainOut), "WARNING") {
		t.Errorf("expected a WARNING about the branch mismatch, got:\n%s", chainOut)
	}

	mainRefs, _ := exec.Command("git", "--git-dir="+remote, "log", "--oneline", "main").CombinedOutput()
	if strings.Contains(string(mainRefs), "add boid project scaffold") {
		t.Errorf("expected the remote's default branch (main) to be UNTOUCHED by the scaffold commit, but it appears in main's history:\n%s", mainRefs)
	}
	refs, _ := exec.Command("git", "--git-dir="+remote, "show-ref").CombinedOutput()
	if !strings.Contains(string(refs), "refs/heads/feature-branch") {
		t.Errorf("expected the scaffold to have been pushed as an ordinary 'feature-branch' ref, show-ref output:\n%s", refs)
	}
}

// withProjectAddWorkspaceFlag sets the package-level --workspace flag value
// `runProjectAdd` reads (projectAddWorkspace) for the duration of the
// calling test, restoring it to "" afterward so this global does not leak
// across other tests in the same binary.
func withProjectAddWorkspaceFlag(t *testing.T, slug string) {
	t.Helper()
	projectAddWorkspace = slug
	t.Cleanup(func() { projectAddWorkspace = "" })
}

// writeGitURLTestProject builds a plain (non-bare) local git repo with a
// COMMITTED .boid/project.yaml, usable as the `boid project add <git-url>`
// argument — unlike writeImportTestProject (task_import_test.go), whose
// project.yaml sits UNCOMMITTED on top of an --allow-empty initial commit
// (fine for the legacy dir-based CreateProject flow, which reads straight
// off disk, but useless as a `git clone` source: only committed content
// travels). A local filesystem path is itself a perfectly valid git URL, so
// this exercises runProjectAdd's real git-clone code path end-to-end
// without a fake forge.
func writeGitURLTestProject(t *testing.T, id, name string) string {
	t.Helper()
	dir := t.TempDir()
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir .boid: %v", err)
	}
	yaml := "id: " + id + "\nname: " + name + "\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	runGitTestCmd(t, dir, "init", "-q", "-b", "main")
	runGitTestCmd(t, dir, "config", "user.email", "test@example.com")
	runGitTestCmd(t, dir, "config", "user.name", "Test")
	runGitTestCmd(t, dir, "add", ".")
	runGitTestCmd(t, dir, "commit", "-q", "-m", "initial")
	return dir
}

// gitFileURL turns a local filesystem path into a "file://" URL —
// looksLikeGitURL (project.go) requires an explicit scheme or the scp-like
// form, so a bare local path (which git itself happily clones) is rejected
// outright by runProjectAdd since PR-4 removed the legacy dir-based form
// (see TestProjectAdd_RejectsHostDirectory). Wrapping test fixture
// directories in file:// lets these tests exercise the git-URL code path
// while still pointing at a local, credential-free source repo.
func gitFileURL(dir string) string { return "file://" + dir }

func runGitTestCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestLooksLikeGitURL pins Minor 1 (PR-2a codex round-2 review): the
// pre-fix strings.Contains(s, "://") / bare-"@...:" heuristic searched
// anywhere in the argument rather than anchoring at the start, so a
// relative path like "./https://repo" was misclassified as a URL, while a
// valid scp-style URL with no explicit login user
// ("host:org/repo.git") was misclassified as a directory.
func TestLooksLikeGitURL(t *testing.T) {
	tests := []struct {
		arg  string
		want bool
	}{
		{"https://github.com/owner/repo.git", true},
		{"ssh://git@github.com/owner/repo.git", true},
		{"git://github.com/owner/repo.git", true},
		{"git@github.com:owner/repo.git", true},
		{"host:org/repo.git", true}, // scp-like, no explicit login user
		{"./my-project", false},
		{"/home/user/my-project", false},
		{"my-project", false},
		{".", false},
		// The two concrete misclassifications codex round-2 named:
		{"./https://repo", false},
		{"./git@host:repo", false},
	}
	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			if got := looksLikeGitURL(tt.arg); got != tt.want {
				t.Errorf("looksLikeGitURL(%q) = %v, want %v", tt.arg, got, tt.want)
			}
		})
	}
}

// TestProjectAdd_RejectsHostDirectory pins PR-4's removal of the legacy
// `boid project add <dir>` form (docs/plans/volume-only-daemon.md §論点e):
// any argument that does not look like a git URL (looksLikeGitURL) is
// rejected client-side, before any daemon round trip, with the exact
// message the plan doc's CLI-scope bullet specifies — pointing the operator
// at the still-working git-URL form rather than silently registering
// nothing or (worse) misinterpreting the path as something else.
func TestProjectAdd_RejectsHostDirectory(t *testing.T) {
	withProjectAddWorkspaceFlag(t, "")

	err := runProjectAdd(projectAddCmd, []string{t.TempDir()})
	if err == nil {
		t.Fatal("expected an error registering a host directory — the legacy form was removed in PR-4")
	}
	if !strings.Contains(err.Error(), "host directory registration was removed") {
		t.Errorf("expected the PR-4 removal message, got: %v", err)
	}
	if !strings.Contains(err.Error(), "boid project add <git-url> --workspace=<name>") {
		t.Errorf("expected the error to point at the git-URL form, got: %v", err)
	}
}

// TestProjectAdd_RejectsHostDirectory_EvenOverRemoteProfile pins that the
// rejection fires purely from the argument's shape (looksLikeGitURL),
// before any profile/transport check runs at all — a remote (non-unix)
// profile gets the identical rejection, not a different "remote profile
// unsupported" message the pre-PR-4 legacy form used to produce.
func TestProjectAdd_RejectsHostDirectory_EvenOverRemoteProfile(t *testing.T) {
	withProjectAddWorkspaceFlag(t, "")

	remoteClient, err := client.NewClient("https://example.invalid", "tok")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	cmd := projectAddCmd
	cmd.SetContext(client.WithClient(context.Background(), remoteClient))
	t.Cleanup(func() { cmd.SetContext(context.Background()) })

	err = runProjectAdd(cmd, []string{t.TempDir()})
	if err == nil {
		t.Fatal("expected an error registering a host directory over a remote profile")
	}
	if !strings.Contains(err.Error(), "host directory registration was removed") {
		t.Errorf("expected the PR-4 removal message, got: %v", err)
	}
}

// TestProjectAdd_RequiresWorkspaceFlag pins the CLI-side half of the
// "--workspace is required" contract (the server-side half is
// internal/api's TestCreateProjectFromGitURL_RequiresWorkspace) — checked
// before any daemon round trip, same as the directory-argument rejection
// above.
func TestProjectAdd_RequiresWorkspaceFlag(t *testing.T) {
	withProjectAddWorkspaceFlag(t, "")

	err := runProjectAdd(projectAddCmd, []string{"https://example.invalid/owner/repo.git"})
	if err == nil {
		t.Fatal("expected an error for a missing --workspace flag")
	}
	if !strings.Contains(err.Error(), "--workspace is required") {
		t.Errorf("expected a --workspace-required error, got: %v", err)
	}
}

// TestProjectAdd_WithUnknownWorkspace_CreatesAndAssigns is the git-URL-flow
// counterpart of MAJOR 4 (codex review, docs/plans/
// workspace-db-consolidation.md): `project add <git-url> --workspace
// <unknown-slug>` must get-or-create an empty workspace DB row for the slug
// client-side (ensureWorkspaceExistsGetOrCreate) before the daemon's own
// CreateProjectFromGitURL — which requires the workspace to already exist,
// unlike the legacy dir-based CreateProject's eager default-workspace
// assign — ever runs.
func TestProjectAdd_WithUnknownWorkspace_CreatesAndAssigns(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	withProjectAddWorkspaceFlag(t, "brand-new-ws")

	src := writeGitURLTestProject(t, "project-add-unknown-ws", "Project Add Unknown WS")

	var out bytes.Buffer
	cmd := projectAddCmd
	cmd.SetOut(&out)
	if err := runProjectAdd(cmd, []string{gitFileURL(src)}); err != nil {
		t.Fatalf("runProjectAdd: %v", err)
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/brand-new-ws", nil, &detail); err != nil {
		t.Fatalf("expected workspace %q to have been get-or-created: %v", "brand-new-ws", err)
	}

	var projects []projectspec.Project
	if err := ts.Client.Do("GET", "/api/projects", nil, &projects); err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected exactly one registered project, got %d", len(projects))
	}
	if projects[0].WorkspaceID != "brand-new-ws" {
		t.Errorf("WorkspaceID = %q, want brand-new-ws", projects[0].WorkspaceID)
	}
	if projects[0].UpstreamURL != gitFileURL(src) {
		t.Errorf("UpstreamURL = %q, want %q", projects[0].UpstreamURL, gitFileURL(src))
	}
}

// TestProjectAdd_WithExistingWorkspace_JustAssigns is the regression guard
// alongside the get-or-create test above: assigning to a slug that already
// has a DB row must not error (no spurious "already exists" 409 surfacing
// from the get-or-create step) and must still assign normally.
func TestProjectAdd_WithExistingWorkspace_JustAssigns(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	testutil.SeedWorkspace(t, ts, "existing-ws")
	withProjectAddWorkspaceFlag(t, "existing-ws")

	src := writeGitURLTestProject(t, "project-add-existing-ws", "Project Add Existing WS")

	var out bytes.Buffer
	cmd := projectAddCmd
	cmd.SetOut(&out)
	if err := runProjectAdd(cmd, []string{gitFileURL(src)}); err != nil {
		t.Fatalf("runProjectAdd: %v", err)
	}

	var projects []projectspec.Project
	if err := ts.Client.Do("GET", "/api/projects", nil, &projects); err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected exactly one registered project, got %d", len(projects))
	}
	if projects[0].WorkspaceID != "existing-ws" {
		t.Errorf("WorkspaceID = %q, want existing-ws", projects[0].WorkspaceID)
	}
}

// TestProjectAdd_NameOverride verifies --name governs the derived project
// name (and thus the bare-repo storage path) independent of the URL's own
// last path component.
func TestProjectAdd_NameOverride(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	withProjectAddWorkspaceFlag(t, "default")
	projectAddName = "custom-name"
	t.Cleanup(func() { projectAddName = "" })

	src := writeGitURLTestProject(t, "project-add-name-override", "Name Override")

	var out bytes.Buffer
	cmd := projectAddCmd
	cmd.SetOut(&out)
	if err := runProjectAdd(cmd, []string{gitFileURL(src)}); err != nil {
		t.Fatalf("runProjectAdd: %v", err)
	}

	if !strings.Contains(out.String(), "custom-name.git") {
		t.Errorf("expected output to reference the overridden bare-repo name, got:\n%s", out.String())
	}
}

// TestProjectFetch_UpdatesProjectYAML exercises `boid project fetch` end to
// end against a real daemon: register via a git URL, change the source
// repo's project.yaml and commit, fetch, and verify the change landed.
func TestProjectFetch_UpdatesProjectYAML(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	withProjectAddWorkspaceFlag(t, "default")

	src := writeGitURLTestProject(t, "project-fetch-test", "Before Fetch")

	var addOut bytes.Buffer
	addCmd := projectAddCmd
	addCmd.SetOut(&addOut)
	if err := runProjectAdd(addCmd, []string{gitFileURL(src)}); err != nil {
		t.Fatalf("runProjectAdd: %v", err)
	}

	if err := os.WriteFile(filepath.Join(src, ".boid", "project.yaml"), []byte("id: project-fetch-test\nname: After Fetch\n"), 0o644); err != nil {
		t.Fatalf("rewrite project.yaml: %v", err)
	}
	runGitTestCmd(t, src, "add", ".")
	runGitTestCmd(t, src, "commit", "-q", "-m", "rename")

	var fetchOut bytes.Buffer
	fetchCmd := projectFetchCmd
	fetchCmd.SetOut(&fetchOut)
	if err := runProjectFetch(fetchCmd, []string{"project-fetch-test"}); err != nil {
		t.Fatalf("runProjectFetch: %v", err)
	}
	if !strings.Contains(fetchOut.String(), "After Fetch") {
		t.Errorf("expected fetch output to reflect the updated name, got:\n%s", fetchOut.String())
	}

	var projects []projectspec.Project
	if err := ts.Client.Do("GET", "/api/projects", nil, &projects); err != nil {
		t.Fatalf("list projects: %v", err)
	}
	if len(projects) != 1 || projects[0].Meta.Name != "After Fetch" {
		t.Fatalf("expected the registered project's name to be updated after fetch, got: %+v", projects)
	}
}
