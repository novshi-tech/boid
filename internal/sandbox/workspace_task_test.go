package sandbox_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestBrokerWorkspaceTaskCreation(t *testing.T) {
	for _, tc := range []struct {
		project, patch, wantProject string
		wantCode                    int
	}{
		{"p1", `{"title":"child"}`, "p1", 0},
		{"p1", `{"title":"child","no_project":true}`, "", 0},
		{"", `{"title":"child"}`, "", 0},
		{"p1", `{"title":"child","no_project":true,"workspace_id":"other"}`, "", 1},
		{"p1", `{"title":"child","no_project":true,"project_id":"p1"}`, "", 1},
	} {
		exec := &fakeBoidExecutor{}
		broker := &sandbox.Broker{BoidExecutor: exec}
		token := broker.Register(map[string]sandbox.CommandDef{}, testGateBoidPolicies(), sandbox.TokenContext{ProjectID: tc.project, WorkspaceID: "team", Role: testRoleGate})
		response := broker.Handle(&sandbox.ExecRequest{Command: "boid", Token: token, Cwd: "/tmp", Boid: &sandbox.BoidRequest{Op: sandbox.BoidOpTaskCreate, CreatePatch: []byte(tc.patch)}})
		if response.ExitCode != tc.wantCode {
			t.Fatalf("%s: %+v", tc.patch, response)
		}
		if tc.wantCode == 0 && (exec.calls[0].ProjectID != tc.wantProject || (tc.wantProject == "" && exec.calls[0].WorkspaceID != "team")) {
			t.Fatalf("request: %+v", exec.calls[0])
		}
	}
}

func TestWorkspaceOnlyTaskAuthorization(t *testing.T) {
	for _, tc := range []struct {
		ctx                sandbox.TokenContext
		project, workspace string
		want               bool
	}{
		{sandbox.TokenContext{ProjectID: "repo", WorkspaceID: "team"}, "", "team", true},
		{sandbox.TokenContext{ProjectID: "repo", WorkspaceID: "team"}, "", "other", false},
		{sandbox.TokenContext{}, "", "", false},
	} {
		if got := tc.ctx.AllowsTask(tc.project, tc.workspace); got != tc.want {
			t.Fatalf("%+v: %v", tc, got)
		}
	}
}
