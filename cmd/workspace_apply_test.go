package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

// resetWorkspaceApplyFlags clears workspaceApplyCmd's -f/--file/--dry-run
// flag state between tests (mirrors resetWorkspaceExportImportFlags's
// rationale — package-level *cobra.Command singletons leak flag state
// across tests otherwise).
func resetWorkspaceApplyFlags(t *testing.T) {
	t.Helper()
	if err := workspaceApplyCmd.Flags().Set("file", ""); err != nil {
		t.Fatalf("reset apply -f: %v", err)
	}
	if err := workspaceApplyCmd.Flags().Set("dry-run", "false"); err != nil {
		t.Fatalf("reset apply --dry-run: %v", err)
	}
}

func writeApplyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "apply.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write apply file: %v", err)
	}
	return path
}

// TestRunWorkspaceApply_CreatesNewWorkspace pins the create branch: applying
// a document for a slug with no existing DB row creates it.
func TestRunWorkspaceApply_CreatesNewWorkspace(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh")
	resetWorkspaceApplyFlags(t)
	defer resetWorkspaceApplyFlags(t)

	file := writeApplyFile(t, `
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-a
spec:
  host_commands: [gh]
  env:
    FOO: bar
`)
	if err := workspaceApplyCmd.Flags().Set("file", file); err != nil {
		t.Fatalf("set -f: %v", err)
	}

	var out bytes.Buffer
	cmd := workspaceApplyCmd
	cmd.SetOut(&out)
	if err := runWorkspaceApply(cmd, nil); err != nil {
		t.Fatalf("runWorkspaceApply: %v", err)
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-a", nil, &detail); err != nil {
		t.Fatalf("verify apply: %v", err)
	}
	if !equalStrSliceForWorkspaceTest(detail.Meta.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want [gh]", detail.Meta.HostCommands)
	}
	if detail.Meta.Env["FOO"] != "bar" {
		t.Errorf("Env[FOO] = %q, want bar", detail.Meta.Env["FOO"])
	}
	if !strings.Contains(out.String(), "created") {
		t.Errorf("stdout = %q, want it to mention creation", out.String())
	}
}

// TestRunWorkspaceApply_MergeSemantics pins the K8s-style merge contract
// (docs/plans/volume-only-daemon.md, PR-1d unilateral decision): a spec
// field absent from the applied document leaves the existing value
// untouched; a present field (even an explicit empty one) replaces it.
func TestRunWorkspaceApply_MergeSemantics(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh", "aws")
	resetWorkspaceCreateEditFlags(t)
	resetWorkspaceApplyFlags(t)
	defer resetWorkspaceApplyFlags(t)

	if err := runWorkspaceCreate(workspaceCreateCmd, []string{"team-b"}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	editFile := writeApplyFile(t, "host_commands:\n  - gh\nenv:\n  FOO: bar\n")
	if err := workspaceEditCmd.Flags().Set("from-file", editFile); err != nil {
		t.Fatalf("set --from-file: %v", err)
	}
	if err := runWorkspaceEdit(workspaceEditCmd, []string{"team-b"}); err != nil {
		t.Fatalf("seed edit: %v", err)
	}
	resetWorkspaceCreateEditFlags(t)

	// This document only mentions env (present, different content) — host_commands
	// is absent and must be left as [gh].
	applyFile := writeApplyFile(t, `
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-b
spec:
  env:
    BAZ: qux
`)
	if err := workspaceApplyCmd.Flags().Set("file", applyFile); err != nil {
		t.Fatalf("set -f: %v", err)
	}

	var out bytes.Buffer
	cmd := workspaceApplyCmd
	cmd.SetOut(&out)
	if err := runWorkspaceApply(cmd, nil); err != nil {
		t.Fatalf("runWorkspaceApply: %v", err)
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-b", nil, &detail); err != nil {
		t.Fatalf("verify apply: %v", err)
	}
	if !equalStrSliceForWorkspaceTest(detail.Meta.HostCommands, []string{"gh"}) {
		t.Errorf("HostCommands = %v, want preserved [gh] (field absent from applied doc)", detail.Meta.HostCommands)
	}
	if detail.Meta.Env["FOO"] != "" {
		t.Errorf("Env[FOO] = %q, want cleared (env field present, replaced wholesale)", detail.Meta.Env["FOO"])
	}
	if detail.Meta.Env["BAZ"] != "qux" {
		t.Errorf("Env[BAZ] = %q, want qux", detail.Meta.Env["BAZ"])
	}
}

// TestRunWorkspaceApply_EmptyEnvClears pins the "present-but-empty clears"
// half of the same contract: "env: {}" removes every existing env entry.
func TestRunWorkspaceApply_EmptyEnvClears(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetWorkspaceCreateEditFlags(t)
	resetWorkspaceApplyFlags(t)
	defer resetWorkspaceApplyFlags(t)

	if err := runWorkspaceCreate(workspaceCreateCmd, []string{"team-c"}); err != nil {
		t.Fatalf("seed create: %v", err)
	}
	editFile := writeApplyFile(t, "env:\n  FOO: bar\n")
	if err := workspaceEditCmd.Flags().Set("from-file", editFile); err != nil {
		t.Fatalf("set --from-file: %v", err)
	}
	if err := runWorkspaceEdit(workspaceEditCmd, []string{"team-c"}); err != nil {
		t.Fatalf("seed edit: %v", err)
	}
	resetWorkspaceCreateEditFlags(t)

	applyFile := writeApplyFile(t, "apiVersion: boid.dev/v1\nkind: Workspace\nmetadata:\n  name: team-c\nspec:\n  env: {}\n")
	if err := workspaceApplyCmd.Flags().Set("file", applyFile); err != nil {
		t.Fatalf("set -f: %v", err)
	}

	var out bytes.Buffer
	cmd := workspaceApplyCmd
	cmd.SetOut(&out)
	if err := runWorkspaceApply(cmd, nil); err != nil {
		t.Fatalf("runWorkspaceApply: %v", err)
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-c", nil, &detail); err != nil {
		t.Fatalf("verify apply: %v", err)
	}
	if len(detail.Meta.Env) != 0 {
		t.Errorf("Env = %v, want cleared", detail.Meta.Env)
	}
}

// TestRunWorkspaceApply_DryRunMakesNoChanges pins --dry-run: the diff is
// printed to stdout, but nothing is written.
func TestRunWorkspaceApply_DryRunMakesNoChanges(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh")
	resetWorkspaceApplyFlags(t)
	defer resetWorkspaceApplyFlags(t)

	file := writeApplyFile(t, `
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-d
spec:
  host_commands: [gh]
`)
	if err := workspaceApplyCmd.Flags().Set("file", file); err != nil {
		t.Fatalf("set -f: %v", err)
	}
	if err := workspaceApplyCmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatalf("set --dry-run: %v", err)
	}

	var out bytes.Buffer
	cmd := workspaceApplyCmd
	cmd.SetOut(&out)
	if err := runWorkspaceApply(cmd, nil); err != nil {
		t.Fatalf("runWorkspaceApply --dry-run: %v", err)
	}

	if err := ts.Client.Do("GET", "/api/workspaces/team-d", nil, &api.WorkspaceDetail{}); err == nil {
		t.Fatal("expected team-d to NOT exist after a --dry-run apply")
	}
	if !strings.Contains(out.String(), "team-d") {
		t.Errorf("dry-run output = %q, want it to mention team-d", out.String())
	}
	if !strings.Contains(out.String(), "host_commands") {
		t.Errorf("dry-run output = %q, want it to mention the changed host_commands field", out.String())
	}
}

// TestRunWorkspaceApply_AttachesExistingProject pins the projects[] attach
// step: a project already registered under the given name is assigned to
// the workspace.
func TestRunWorkspaceApply_AttachesExistingProject(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetWorkspaceApplyFlags(t)
	defer resetWorkspaceApplyFlags(t)

	dir := writeImportTestProject(t, "rook-server", "Rook Server")
	var project orchestrator.Project
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}
	name := "Rook Server" // project.yaml's "name:" field — see projectExportName's doc comment.

	file := writeApplyFile(t, `
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-e
spec:
  projects:
    - name: `+name+`
`)
	if err := workspaceApplyCmd.Flags().Set("file", file); err != nil {
		t.Fatalf("set -f: %v", err)
	}

	var out bytes.Buffer
	cmd := workspaceApplyCmd
	cmd.SetOut(&out)
	if err := runWorkspaceApply(cmd, nil); err != nil {
		t.Fatalf("runWorkspaceApply: %v", err)
	}

	var updated orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects/"+project.ID, nil, &updated); err != nil {
		t.Fatalf("get project after apply: %v", err)
	}
	if updated.WorkspaceID != "team-e" {
		t.Errorf("WorkspaceID = %q, want team-e", updated.WorkspaceID)
	}
}

// TestRunWorkspaceApply_WarnsOnMissingProject pins the exact warning wording
// (docs/plans/volume-only-daemon.md §論点g) for a projects[] entry that does
// not resolve to a registered project — apply must still succeed.
func TestRunWorkspaceApply_WarnsOnMissingProject(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetWorkspaceApplyFlags(t)
	defer resetWorkspaceApplyFlags(t)

	file := writeApplyFile(t, `
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-f
spec:
  projects:
    - name: ghost-project
`)
	if err := workspaceApplyCmd.Flags().Set("file", file); err != nil {
		t.Fatalf("set -f: %v", err)
	}

	var out, errOut bytes.Buffer
	cmd := workspaceApplyCmd
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := runWorkspaceApply(cmd, nil); err != nil {
		t.Fatalf("runWorkspaceApply: %v", err)
	}

	want := "warning: project 'ghost-project' referenced in workspace 'team-f' does not exist yet. Register it separately (or via PR-2's boid project add <url>) then re-apply."
	if !strings.Contains(errOut.String(), want) {
		t.Errorf("stderr = %q, want it to contain %q", errOut.String(), want)
	}

	// team-f itself must still have been created despite the missing project.
	if err := ts.Client.Do("GET", "/api/workspaces/team-f", nil, &api.WorkspaceDetail{}); err != nil {
		t.Errorf("expected team-f to be created despite the missing project warning: %v", err)
	}
}

// TestRunWorkspaceApply_WarnsOnAdditionalBindingsDrop pins the tolerate-
// and-warn contract for the retired additional_bindings field.
func TestRunWorkspaceApply_WarnsOnAdditionalBindingsDrop(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetWorkspaceApplyFlags(t)
	defer resetWorkspaceApplyFlags(t)

	file := writeApplyFile(t, `
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-g
spec:
  additional_bindings:
    - source: /host/path
      target: /container/path
`)
	if err := workspaceApplyCmd.Flags().Set("file", file); err != nil {
		t.Fatalf("set -f: %v", err)
	}

	var out, errOut bytes.Buffer
	cmd := workspaceApplyCmd
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	if err := runWorkspaceApply(cmd, nil); err != nil {
		t.Fatalf("runWorkspaceApply: %v", err)
	}
	if !strings.Contains(errOut.String(), "additional_bindings") {
		t.Errorf("stderr = %q, want a warning mentioning additional_bindings", errOut.String())
	}
	if err := ts.Client.Do("GET", "/api/workspaces/team-g", nil, &api.WorkspaceDetail{}); err != nil {
		t.Errorf("expected team-g to be created despite the additional_bindings warning: %v", err)
	}
}

// TestRunWorkspaceApply_MultiDocument pins "---"-separated multi-document
// apply files: every document is applied.
func TestRunWorkspaceApply_MultiDocument(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	resetWorkspaceApplyFlags(t)
	defer resetWorkspaceApplyFlags(t)

	file := writeApplyFile(t, `
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-h
---
apiVersion: boid.dev/v1
kind: Workspace
metadata:
  name: team-i
`)
	if err := workspaceApplyCmd.Flags().Set("file", file); err != nil {
		t.Fatalf("set -f: %v", err)
	}

	var out bytes.Buffer
	cmd := workspaceApplyCmd
	cmd.SetOut(&out)
	if err := runWorkspaceApply(cmd, nil); err != nil {
		t.Fatalf("runWorkspaceApply: %v", err)
	}

	for _, slug := range []string{"team-h", "team-i"} {
		if err := ts.Client.Do("GET", "/api/workspaces/"+slug, nil, &api.WorkspaceDetail{}); err != nil {
			t.Errorf("expected workspace %q to exist: %v", slug, err)
		}
	}
}

// TestRunWorkspaceApply_RequiresFileFlag pins that -f/--file is mandatory.
func TestRunWorkspaceApply_RequiresFileFlag(t *testing.T) {
	resetWorkspaceApplyFlags(t)
	defer resetWorkspaceApplyFlags(t)
	err := runWorkspaceApply(workspaceApplyCmd, nil)
	if err == nil {
		t.Fatal("expected an error when -f/--file is not given")
	}
}

// TestWorkspaceRemove_DeleteAlias pins that `boid workspace delete` is
// registered as an alias of `boid workspace remove` (docs/plans/
// volume-only-daemon.md §論点g's CLI surface) rather than a separate
// implementation — same semantics, same tests, one alias.
func TestWorkspaceRemove_DeleteAlias(t *testing.T) {
	found := false
	for _, alias := range workspaceRemoveCmd.Aliases {
		if alias == "delete" {
			found = true
		}
	}
	if !found {
		t.Errorf("workspaceRemoveCmd.Aliases = %v, want it to include \"delete\"", workspaceRemoveCmd.Aliases)
	}
	cmd, _, err := workspaceCmd.Find([]string{"delete", "some-slug"})
	if err != nil {
		t.Fatalf("workspaceCmd.Find([delete ...]): %v", err)
	}
	if cmd != workspaceRemoveCmd {
		t.Errorf("workspaceCmd.Find([delete ...]) resolved to %v, want workspaceRemoveCmd", cmd)
	}
}
