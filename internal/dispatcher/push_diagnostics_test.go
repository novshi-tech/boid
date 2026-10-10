package dispatcher

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestPushRequestedRefsPersistInTaskTimeline(t *testing.T) {
	d := newGatewayTestDB(t)
	task := &orchestrator.Task{ID: "task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{BaseBranch: "main"}, ProjectID: "project", Status: orchestrator.TaskStatusExecuting}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "project", WorkDir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatal(err)
	}
	r := &Runner{DB: d.Conn, GitGateway: gitgateway.NewRegistry()}
	spec := &orchestrator.JobSpec{TaskID: task.ID, ProjectID: "project", Env: map[string]string{"BOID_BASE_BRANCH": "main"}}
	_, token := r.registerGatewayToken("job", spec, "")
	entry, ok := r.GitGateway.Lookup(token)
	if !ok || entry.ObservePush == nil {
		t.Fatal("no push observer")
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/boid/12345678", "refs/heads/other"} {
		entry.ObservePush("github.com/owner/repo", ref)
	}
	actions, err := orchestrator.ListActionsByTask(d.Conn, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 3 {
		t.Fatalf("actions=%d", len(actions))
	}
	for i, ref := range []string{"refs/heads/main", "refs/heads/boid/12345678", "refs/heads/other"} {
		var data map[string]string
		if err := json.Unmarshal(actions[i].Payload, &data); err != nil {
			t.Fatal(err)
		}
		if actions[i].Type != "progress" || data["ref"] != ref || data["job_id"] != "job" || data["base_branch"] != "main" {
			t.Fatalf("diagnostic=%v", data)
		}
		if strings.Contains(data["message"], "violation") {
			t.Fatal("observation must not impose a branch policy")
		}
	}
	current, err := orchestrator.GetTask(d.Conn, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != task.Status {
		t.Fatalf("diagnostics changed status to %s", current.Status)
	}
}
