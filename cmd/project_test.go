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
	if !strings.Contains(got, "boid project add <git-url>") {
		t.Errorf("expected guidance to point at `boid project add <git-url>`, got:\n%s", got)
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

// TestProjectInit_ExistingOrigin_GuidesRegisterWithThatURL pins the
// smarter branch: when projectDir is already a git repo with an `origin`
// remote configured (e.g. the user ran `git init` themselves before
// `boid project init`), the guidance should skip the "git init" step and
// go straight to "commit + push" using the ALREADY-KNOWN URL, rather than
// printing a generic <git-url> placeholder the user has to fill in by hand.
func TestProjectInit_ExistingOrigin_GuidesRegisterWithThatURL(t *testing.T) {
	dir := t.TempDir()
	runGitTestCmd(t, dir, "init", "-q", "-b", "main")
	runGitTestCmd(t, dir, "config", "user.email", "test@example.com")
	runGitTestCmd(t, dir, "config", "user.name", "Test")
	runGitTestCmd(t, dir, "commit", "-q", "--allow-empty", "-m", "initial")
	originURL := "https://example.invalid/owner/repo.git"
	runGitTestCmd(t, dir, "remote", "add", "origin", originURL)

	withStdin(t, devNullStdin(t))

	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := runProjectInit(cmd, []string{dir}); err != nil {
		t.Fatalf("runProjectInit: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, originURL) {
		t.Errorf("expected guidance to reference the existing origin URL %q, got:\n%s", originURL, got)
	}
	if strings.Contains(got, "git remote add origin") {
		t.Errorf("did not expect a 'git remote add origin' instruction when origin already exists, got:\n%s", got)
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
	wantCd := "cd '" + dir + "' && git init"
	if !strings.Contains(got, wantCd) {
		t.Errorf("expected guidance to contain %q, got:\n%s", wantCd, got)
	}
}

// TestProjectInit_ExistingOrigin_PushUsesUpstreamFlag pins Blocker 2 (codex
// round-1 review of this PR): a repo whose only git setup so far is `git
// init` + `git remote add origin <url>` (project init's own documented
// "skip what's already done" scenario) has NO upstream tracking branch
// yet, so a bare `git push` fails with "no upstream branch configured".
// The guidance must use `git push -u origin HEAD` instead.
func TestProjectInit_ExistingOrigin_PushUsesUpstreamFlag(t *testing.T) {
	dir := t.TempDir()
	runGitTestCmd(t, dir, "init", "-q", "-b", "main")
	runGitTestCmd(t, dir, "config", "user.email", "test@example.com")
	runGitTestCmd(t, dir, "config", "user.name", "Test")
	runGitTestCmd(t, dir, "commit", "-q", "--allow-empty", "-m", "initial")
	runGitTestCmd(t, dir, "remote", "add", "origin", "https://example.invalid/owner/repo.git")

	withStdin(t, devNullStdin(t))

	var out bytes.Buffer
	cmd := projectInitSubCmd
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())

	if err := runProjectInit(cmd, []string{dir}); err != nil {
		t.Fatalf("runProjectInit: %v", err)
	}

	if !strings.Contains(out.String(), "git push -u origin HEAD") {
		t.Errorf("expected guidance to use 'git push -u origin HEAD', got:\n%s", out.String())
	}
}

// TestProjectInit_NestedInDifferentRepo_DoesNotMisattributeParentOrigin
// pins Blocker 1's second half (codex round-1 review of this PR): scaffold
// a NEW subdirectory (with no .git of its own) inside an ALREADY-git-init'd
// parent that has its own, unrelated origin. A bare `git config --get
// remote.origin.url` run with cwd=subdir walks UP to the parent's .git and
// reports the parent's origin — misattributing it to the freshly scaffolded
// subdirectory, which is not what got pushed at all. The guidance must
// fall back to the "no origin" branch (git init/remote add) instead of
// printing the parent's URL.
func TestProjectInit_NestedInDifferentRepo_DoesNotMisattributeParentOrigin(t *testing.T) {
	parent := t.TempDir()
	runGitTestCmd(t, parent, "init", "-q", "-b", "main")
	runGitTestCmd(t, parent, "config", "user.email", "test@example.com")
	runGitTestCmd(t, parent, "config", "user.name", "Test")
	runGitTestCmd(t, parent, "commit", "-q", "--allow-empty", "-m", "initial")
	parentOriginURL := "https://example.invalid/owner/unrelated-parent-repo.git"
	runGitTestCmd(t, parent, "remote", "add", "origin", parentOriginURL)

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
	if strings.Contains(got, parentOriginURL) {
		t.Errorf("must not attribute the parent repo's origin %q to the nested scaffold dir, got:\n%s", parentOriginURL, got)
	}
	if !strings.Contains(got, "git remote add origin") {
		t.Errorf("expected the no-origin branch (git remote add origin) for a dir with no .git of its own, got:\n%s", got)
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
