package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
	"github.com/spf13/cobra"
)

func TestWorkspaceIdentityCLIAndAPI(t *testing.T) {
	ts := testutil.NewTestServer(t)
	for _, slug := range []string{"team", "other"} {
		if err := ts.Client.DoWithContentType("POST", "/api/workspaces", "application/yaml", []byte("slug: "+slug+"\n"), nil); err != nil {
			t.Fatal(err)
		}
	}
	out := executeIdentityTestCmd(t, ts, "task", "resolve-or-capture", "jira:1", "--workspace", "team", "--title", "Card")
	var capture api.ResolveOrCaptureResult
	if err := json.Unmarshal([]byte(out), &capture); err != nil || !capture.Created {
		t.Fatalf("capture: %s %v", out, err)
	}
	out = executeIdentityTestCmd(t, ts, "task", "resolve-or-capture", "jira:1", "--workspace", "team")
	var repeat api.ResolveOrCaptureResult
	if err := json.Unmarshal([]byte(out), &repeat); err != nil || repeat.Created || repeat.TaskID != capture.TaskID {
		t.Fatalf("repeat: %s %v", out, err)
	}
	var card orchestrator.Task
	if err := ts.Client.Do("GET", "/api/tasks/"+capture.TaskID, nil, &card); err != nil {
		t.Fatal(err)
	}
	if card.ProjectID != "" || card.WorkspaceID != "team" {
		t.Fatalf("card: %+v", card)
	}
	executeIdentityTestCmd(t, ts, "task", "identity", "link", "jira:2", card.ID, "--workspace", "team")
	out = executeIdentityTestCmd(t, ts, "task", "identity", "resolve", "jira:2", "--workspace", "team")
	var resolved struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(out), &resolved); err != nil || resolved.TaskID != card.ID {
		t.Fatalf("resolve: %s %v", out, err)
	}
	executeIdentityTestCmd(t, ts, "task", "identity", "unlink", "jira:2", "--workspace", "team")
	if err := ts.Client.Do("POST", "/api/tasks/identity/resolve", api.TaskIdentityRequest{WorkspaceID: "team", Identity: "jira:2"}, nil); err == nil {
		t.Fatal("unlinked identity resolved")
	}
	if err := ts.Client.Do("POST", "/api/tasks/identity/link", api.TaskIdentityRequest{WorkspaceID: "other", Identity: "jira:3", TaskID: card.ID}, nil); err == nil {
		t.Fatal("cross-workspace identity linked")
	}
}

func executeIdentityTestCmd(t *testing.T, ts *testutil.TestServer, args ...string) string {
	t.Helper()
	operation := args[1]
	args = args[2:]
	if operation == "identity" {
		operation = args[0]
		args = args[1:]
	} else {
		operation = "capture"
	}
	cmd := &cobra.Command{}
	addIdentityScopeFlags(cmd)
	addIdentityMetadataFlags(cmd)
	cmd.Flags().String("description-file", "", "")
	cmd.Flags().String("title", "", "")
	cmd.Flags().String("description", "", "")
	cmd.SetContext(client.WithClient(context.Background(), ts.Client))
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := cmd.Flags().Parse(args); err != nil {
		t.Fatal(err)
	}
	if err := runIdentityCommand(cmd, cmd.Flags().Args(), operation); err != nil {
		t.Fatal(err)
	}
	return output.String()
}
