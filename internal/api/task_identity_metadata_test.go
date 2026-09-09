package api

import (
	"context"
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func newIdentityMetadataService(t *testing.T) (*TaskAppService, *realTransactor, *orchestrator.Task) {
	t.Helper()
	workflow := newResolveOrCaptureTestService(t)
	tx := workflow.Tx.(realTransactor)
	task := &orchestrator.Task{
		ProjectID: "proj-1",
		Title:     "metadata target",
		Type:      orchestrator.TaskTypeExecution,
		Exec:      &orchestrator.ExecAttrs{Behavior: "dev", Payload: []byte(`{}`)},
	}
	if err := orchestrator.CreateTask(tx.conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}
	return &TaskAppService{Tx: tx}, &tx, task
}

func TestLinkIdentityWithMetadata_InvalidURLDoesNotCreateBinding(t *testing.T) {
	svc, tx, task := newIdentityMetadataService(t)
	badURL := "javascript:alert(1)"

	err := svc.LinkIdentityWithMetadata(context.Background(), "proj-1", "jira:BAD-1", task.ID, &badURL, nil)
	if err == nil {
		t.Fatal("LinkIdentityWithMetadata returned nil for a non-HTTP(S) URL")
	}
	if _, err := orchestrator.ResolveIdentity(tx.conn, "proj-1", "jira:BAD-1"); !errors.Is(err, orchestrator.ErrTaskNotFound) {
		t.Fatalf("binding after rejected URL: err = %v, want ErrTaskNotFound", err)
	}
}

func TestLinkIdentityWithMetadata_MetadataFailureRollsBackBinding(t *testing.T) {
	svc, tx, task := newIdentityMetadataService(t)
	if _, err := tx.conn.Exec(`CREATE TRIGGER reject_identity_metadata
		BEFORE UPDATE OF url, display_name ON task_identities
		BEGIN SELECT RAISE(FAIL, 'forced metadata failure'); END`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}
	resourceURL := "https://jira.example/browse/FAIL-1"

	err := svc.LinkIdentityWithMetadata(context.Background(), "proj-1", "jira:FAIL-1", task.ID, &resourceURL, nil)
	if err == nil {
		t.Fatal("LinkIdentityWithMetadata returned nil for forced metadata failure")
	}
	if _, err := orchestrator.ResolveIdentity(tx.conn, "proj-1", "jira:FAIL-1"); !errors.Is(err, orchestrator.ErrTaskNotFound) {
		t.Fatalf("binding survived failed metadata update: err = %v, want ErrTaskNotFound", err)
	}
}

func TestLinkIdentityWithMetadata_WithoutTransactionDoesNotCreateBinding(t *testing.T) {
	_, tx, task := newIdentityMetadataService(t)
	repo := orchestrator.NewTaskRepository(tx.conn)
	svc := &TaskAppService{Tasks: repo, Actions: repo, Identities: repo}
	resourceURL := "https://jira.example/browse/NO-TX-1"

	err := svc.LinkIdentityWithMetadata(context.Background(), "proj-1", "jira:NO-TX-1", task.ID, &resourceURL, nil)
	if !errors.Is(err, errIdentityMetadataUnavailable) {
		t.Fatalf("LinkIdentityWithMetadata error = %v, want metadata transaction unavailable", err)
	}
	if _, err := orchestrator.ResolveIdentity(tx.conn, "proj-1", "jira:NO-TX-1"); !errors.Is(err, orchestrator.ErrTaskNotFound) {
		t.Fatalf("binding after no-transaction rejection: err = %v, want ErrTaskNotFound", err)
	}
}
