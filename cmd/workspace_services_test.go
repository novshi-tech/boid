package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestRunWorkspaceServices_AddListRemove_FullCycle(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	testutil.SeedWorkspace(t, ts, "team-a")

	// list: empty at first.
	var listOut bytes.Buffer
	listCmd := workspaceServicesListCmd
	listCmd.SetOut(&listOut)
	if err := runWorkspaceServicesList(listCmd, []string{"team-a"}); err != nil {
		t.Fatalf("runWorkspaceServicesList (empty): %v", err)
	}
	if strings.Contains(listOut.String(), "myapp") {
		t.Errorf("expected no services yet, got %q", listOut.String())
	}

	// add two services.
	var addOut bytes.Buffer
	addCmd := workspaceServicesAddCmd
	addCmd.SetOut(&addOut)
	if err := runWorkspaceServicesAdd(addCmd, []string{"team-a", "myapp", "myapp-ops"}); err != nil {
		t.Fatalf("runWorkspaceServicesAdd: %v", err)
	}
	if !strings.Contains(addOut.String(), "myapp") || !strings.Contains(addOut.String(), "myapp-ops") {
		t.Errorf("add output = %q, want both service names", addOut.String())
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-a", nil, &detail); err != nil {
		t.Fatalf("verify add: %v", err)
	}
	if len(detail.Meta.Services) != 2 {
		t.Fatalf("Services after add = %v, want 2 entries", detail.Meta.Services)
	}

	// adding the same service again is idempotent (no duplicate).
	var addAgainOut bytes.Buffer
	addCmd.SetOut(&addAgainOut)
	if err := runWorkspaceServicesAdd(addCmd, []string{"team-a", "myapp"}); err != nil {
		t.Fatalf("runWorkspaceServicesAdd (dup): %v", err)
	}
	if err := ts.Client.Do("GET", "/api/workspaces/team-a", nil, &detail); err != nil {
		t.Fatalf("verify dup add: %v", err)
	}
	if len(detail.Meta.Services) != 2 {
		t.Errorf("Services after duplicate add = %v, want still 2 entries (no dup)", detail.Meta.Services)
	}

	// list now shows both.
	var listOut2 bytes.Buffer
	listCmd.SetOut(&listOut2)
	if err := runWorkspaceServicesList(listCmd, []string{"team-a"}); err != nil {
		t.Fatalf("runWorkspaceServicesList: %v", err)
	}
	if !strings.Contains(listOut2.String(), "myapp") || !strings.Contains(listOut2.String(), "myapp-ops") {
		t.Errorf("list output = %q, want both service names", listOut2.String())
	}

	// remove one.
	var removeOut bytes.Buffer
	removeCmd := workspaceServicesRemoveCmd
	removeCmd.SetOut(&removeOut)
	if err := runWorkspaceServicesRemove(removeCmd, []string{"team-a", "myapp-ops"}); err != nil {
		t.Fatalf("runWorkspaceServicesRemove: %v", err)
	}
	if err := ts.Client.Do("GET", "/api/workspaces/team-a", nil, &detail); err != nil {
		t.Fatalf("verify remove: %v", err)
	}
	if len(detail.Meta.Services) != 1 || detail.Meta.Services[0] != "myapp" {
		t.Errorf("Services after remove = %v, want [myapp]", detail.Meta.Services)
	}

	// removing an entry that isn't there is a no-op, not an error.
	if err := runWorkspaceServicesRemove(removeCmd, []string{"team-a", "nonexistent"}); err != nil {
		t.Fatalf("runWorkspaceServicesRemove (nonexistent): %v", err)
	}
}

// TestAddServiceNames_TrimsWhitespace / TestRemoveServiceNames_TrimsWhitespace
// pin a codex review finding: orchestrator.ResolveEnabledServices trims
// whitespace from each stored entry at dispatch-resolution time, so a
// stored " foo " entry effectively enabled "foo" — but this CLI's own
// add/remove previously compared raw, untrimmed strings, so `services
// remove foo` could never match a " foo " entry (it would silently no-op,
// leaving the untrimmed entry in place forever). Both functions now trim
// before comparing/storing, so a value this CLI ever WRITES is already
// normalized.
func TestAddServiceNames_TrimsWhitespace(t *testing.T) {
	got := addServiceNames([]string{" foo "}, []string{"bar"})
	want := []string{"foo", "bar"}
	if len(got) != len(want) {
		t.Fatalf("addServiceNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("addServiceNames = %v, want %v", got, want)
		}
	}
}

func TestRemoveServiceNames_TrimsWhitespace(t *testing.T) {
	got := removeServiceNames([]string{" foo ", "bar"}, []string{"foo"})
	want := []string{"bar"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("removeServiceNames([\" foo \", \"bar\"], [\"foo\"]) = %v, want %v (untrimmed stored entry must still be removable by its trimmed name)", got, want)
	}
}

func TestRunWorkspaceServices_Add_InvalidSlugRejected(t *testing.T) {
	if err := runWorkspaceServicesAdd(workspaceServicesAddCmd, []string{"Not A Slug", "myapp"}); err == nil {
		t.Fatal("want error for invalid slug, got nil")
	}
}

func TestRunWorkspaceServices_List_PreservesOtherMetaFields(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	seedHostCommandsForTest(t, ts, "gh")

	// Seed host_commands directly via the repository, same as
	// testutil.SeedWorkspace does for an empty meta.
	repo := orchestrator.NewWorkspaceRepository(ts.Server.DB())
	if err := repo.Save("team-a", &orchestrator.WorkspaceMeta{HostCommands: []string{"gh"}}); err != nil {
		t.Fatalf("seed host_commands: %v", err)
	}

	addCmd := workspaceServicesAddCmd
	addCmd.SetOut(&bytes.Buffer{})
	if err := runWorkspaceServicesAdd(addCmd, []string{"team-a", "myapp"}); err != nil {
		t.Fatalf("runWorkspaceServicesAdd: %v", err)
	}

	var detail api.WorkspaceDetail
	if err := ts.Client.Do("GET", "/api/workspaces/team-a", nil, &detail); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if len(detail.Meta.HostCommands) != 1 || detail.Meta.HostCommands[0] != "gh" {
		t.Errorf("host_commands was clobbered by services add: got %v, want [gh]", detail.Meta.HostCommands)
	}
	if len(detail.Meta.Services) != 1 || detail.Meta.Services[0] != "myapp" {
		t.Errorf("Services = %v, want [myapp]", detail.Meta.Services)
	}
}

// TestRunWorkspaceServices_Add_PreservesTaskBehaviorsAndBaseBranch pins a
// regression: the workspace-default-project fields (task_behaviors /
// base_branch / fork_point / default_task_behavior — WorkspaceMeta.
// TaskBehaviors etc., docs/plans/workspace-default-project.md) have no
// field on workspaceMetaStrict (the type PUT /api/workspaces/{slug} decodes
// through — see that type's own "PR3 の deliberate scope boundary" doc
// comment). A first cut of mutateWorkspaceServices round-tripped the FULL
// meta through that PUT endpoint (GET whole meta -> mutate Services ->
// PUT whole meta back) and 400'd outright ("field task_behaviors not found
// in type orchestrator.workspaceMetaStrict") for any workspace carrying
// these fields — confirmed empirically before the fix landed. The fix
// switched to POST /api/workspaces/apply (the envelope path), submitting a
// document whose spec carries ONLY the "services" key so
// decodeWorkspaceEnvelopeSpec's FieldsPresent gate leaves every other field
// untouched. This test seeds a workspace with task_behaviors and
// base_branch set (as `boid workspace apply` would) and asserts `services
// add` neither errors nor clears them.
func TestRunWorkspaceServices_Add_PreservesTaskBehaviorsAndBaseBranch(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	repo := orchestrator.NewWorkspaceRepository(ts.Server.DB())
	if err := repo.Save("team-a", &orchestrator.WorkspaceMeta{
		TaskBehaviors: map[string]orchestrator.TaskBehavior{"dev": {}},
		BaseBranch:    "main",
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	addCmd := workspaceServicesAddCmd
	addCmd.SetOut(&bytes.Buffer{})
	if err := runWorkspaceServicesAdd(addCmd, []string{"team-a", "myapp"}); err != nil {
		t.Fatalf("runWorkspaceServicesAdd: %v (must not 400 on a workspace with task_behaviors/base_branch set)", err)
	}

	got, err := repo.Load("team-a")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := got.TaskBehaviors["dev"]; !ok {
		t.Errorf("TaskBehaviors = %v, want \"dev\" entry preserved", got.TaskBehaviors)
	}
	if got.BaseBranch != "main" {
		t.Errorf("BaseBranch = %q, want %q (must not be cleared by services add)", got.BaseBranch, "main")
	}
	if len(got.Services) != 1 || got.Services[0] != "myapp" {
		t.Errorf("Services = %v, want [myapp]", got.Services)
	}
}
