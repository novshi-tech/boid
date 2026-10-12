package api

import (
	"context"
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestWorkspaceIdentityCaptureLinkResolveUnlink(t *testing.T) {
	workflow := newResolveOrCaptureTestService(t)
	conn := workflow.Tx.(realTransactor).conn
	ws := orchestrator.NewWorkspaceRepository(conn)
	for _, slug := range []string{"team", "other"} {
		if err := ws.Save(slug, &orchestrator.WorkspaceMeta{}); err != nil {
			t.Fatal(err)
		}
	}
	repo := orchestrator.NewTaskRepository(conn)
	service := &TaskAppService{Tasks: repo, Actions: repo, Identities: repo, Tx: workflow.Tx}
	req := ResolveOrCaptureRequest{WorkspaceID: "team", Identity: "jira:1", Title: "Card", URL: ptrString("https://example.com/1"), DisplayName: ptrString("Issue")}
	first, err := workflow.ResolveOrCapture(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := workflow.ResolveOrCapture(context.Background(), req)
	if err != nil || again.Created || again.TaskID != first.TaskID {
		t.Fatalf("duplicate: %+v %v", again, err)
	}
	card, err := repo.GetTask(first.TaskID)
	if err != nil || card.ProjectID != "" || card.WorkspaceID != "team" {
		t.Fatalf("card: %+v %v", card, err)
	}
	metadata, err := repo.ListIdentityMetadataByTask(card.ID)
	if err != nil || len(metadata) != 1 || metadata[0].URL != *req.URL {
		t.Fatalf("metadata: %+v %v", metadata, err)
	}
	otherReq := req
	otherReq.WorkspaceID = "other"
	other, err := workflow.ResolveOrCapture(context.Background(), otherReq)
	if err != nil || other.TaskID == first.TaskID {
		t.Fatalf("workspace scope: %+v %v", other, err)
	}
	if err := service.LinkWorkspaceIdentity(context.Background(), "team", "jira:2", card.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.LinkWorkspaceIdentity(context.Background(), "team", "jira:2", other.TaskID); err == nil {
		t.Fatal("cross-workspace link accepted")
	}
	resolved, err := service.ResolveWorkspaceIdentity("team", "jira:2")
	if err != nil || resolved.ID != card.ID {
		t.Fatalf("resolve: %+v %v", resolved, err)
	}
	if err := service.UnlinkWorkspaceIdentity(context.Background(), "team", "jira:2"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResolveWorkspaceIdentity("team", "jira:2"); !errors.Is(err, orchestrator.ErrTaskNotFound) {
		t.Fatalf("unlink: %v", err)
	}
	var bad int
	if err := conn.QueryRow(`SELECT count(*) FROM task_identities WHERE project_id IS NOT NULL`).Scan(&bad); err != nil || bad != 0 {
		t.Fatalf("identity repository: %d %v", bad, err)
	}
}
