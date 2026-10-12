package cmd

import (
	"bytes"
	"context"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/spf13/cobra"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// No HTTP round trip should be attempted without a project or workspace scope.
func TestRunTriggerRun_RequiresScope(t *testing.T) {
	origProject, origWorkspace := triggerRunProjectRef, triggerRunWorkspace
	triggerRunProjectRef, triggerRunWorkspace = "", ""
	defer func() { triggerRunProjectRef, triggerRunWorkspace = origProject, origWorkspace }()

	err := runTriggerRun(triggerRunCmd, []string{"intake"})
	if err == nil {
		t.Fatal("runTriggerRun() with no -p/--project = nil error, want a rejection")
	}
	if !strings.Contains(err.Error(), "--project") {
		t.Errorf("error = %v, want it to mention --project", err)
	}
}

func TestRunTriggerRun_Workspace(t *testing.T) {
	origProject, origWorkspace := triggerRunProjectRef, triggerRunWorkspace
	t.Cleanup(func() { triggerRunProjectRef, triggerRunWorkspace = origProject, origWorkspace })
	triggerRunProjectRef, triggerRunWorkspace = "", "team"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.EscapedPath() != "/api/workspaces/team/triggers/signal:demo%2Fpoll/run" {
			t.Errorf("request: %s %s", r.Method, r.URL.EscapedPath())
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"job_id":"job-1"}`))
	}))
	defer srv.Close()
	c, err := client.NewClient(srv.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{}
	command.SetContext(client.WithClient(context.Background(), c))
	var out bytes.Buffer
	command.SetOut(&out)
	if err := runTriggerRun(command, []string{"signal:demo/poll"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "fired: job job-1") {
		t.Fatalf("output: %s", out.String())
	}
	triggerRunProjectRef = "project"
	if err := runTriggerRun(command, []string{"sweep"}); err == nil {
		t.Fatal("project and workspace flags accepted together")
	}
}
