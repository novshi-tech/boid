package sandbox_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestBrokerWorkspaceIdentityOperations(t *testing.T) {
	for _, op := range []sandbox.BoidOp{sandbox.BoidOpTaskIdentityLink, sandbox.BoidOpTaskIdentityUnlink, sandbox.BoidOpTaskIdentityResolve, sandbox.BoidOpTaskResolveOrCapture} {
		t.Run(string(op), func(t *testing.T) {
			broker, exec := newBrokerForListTest(t)
			token := broker.Register(map[string]sandbox.CommandDef{}, identityBoidPolicies(), sandbox.TokenContext{JobID: "job", TaskID: "task", WorkspaceID: "team"})
			resp := broker.Handle(&sandbox.ExecRequest{Command: "boid", Cwd: "/tmp", Token: token, Boid: &sandbox.BoidRequest{Op: op, Identity: "jira:1", TaskID: "card"}})
			if resp.ExitCode != 0 || len(exec.calls) != 1 {
				t.Fatalf("workspace operation rejected: %+v", resp)
			}
			if exec.calls[0].ProjectID != "" || exec.calls[0].WorkspaceID != "team" {
				t.Fatalf("scope: %+v", exec.calls[0])
			}
			resp = broker.Handle(&sandbox.ExecRequest{Command: "boid", Cwd: "/tmp", Token: token, Boid: &sandbox.BoidRequest{Op: op, Identity: "jira:1", TaskID: "card", WorkspaceID: "other"}})
			if resp.ExitCode == 0 || len(exec.calls) != 1 {
				t.Fatal("cross-workspace scope accepted")
			}
		})
	}
}
