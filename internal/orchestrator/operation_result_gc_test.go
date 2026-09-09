package orchestrator

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
)

func TestEarlyRequestGCPreservesReceiptDestination(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Conn.Exec(`INSERT INTO projects(id,work_dir) VALUES('p','/tmp/p')`); err != nil {
		t.Fatal(err)
	}
	if err := CreateTask(d.Conn, &Task{ID: "card", ProjectID: "p", Type: TaskTypeCard, Status: TaskStatusParked, Card: &CardAttrs{}}); err != nil {
		t.Fatal(err)
	}
	req := &CardRequest{ID: "req", CardID: "card", CommandKey: "discuss", Status: CardRequestStatusLaunching, TargetKind: CardRequestTargetKindSession, TargetID: "session", LauncherJobID: "launcher", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if err := CreateCardRequest(d.Conn, req); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Conn.Exec(`UPDATE card_requests SET status='finished' WHERE id='req'`); err != nil {
		t.Fatal(err)
	}
	store := NewOperationResultStore(d.Conn)
	receipt := &OperationResult{TaskID: "card", OperationType: "card_command:discuss", Result: OperationResultAccepted, TargetRequestID: "req"}
	if err := store.CreateOperationResult(receipt); err != nil {
		t.Fatal(err)
	}
	if n, err := GCCardRequests(d.Conn, 0, true); err != nil || n != 1 {
		t.Fatalf("dry run: %d %v", n, err)
	}
	var saved string
	if err := d.Conn.QueryRow(`SELECT target_session_id FROM operation_results WHERE id=?`, receipt.ID).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if saved != "" {
		t.Fatal("dry run mutated receipt")
	}
	if n, err := GCCardRequests(d.Conn, 0, false); err != nil || n != 1 {
		t.Fatalf("gc: %d %v", n, err)
	}
	got, err := store.GetOperationResult("card", receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetSessionID != "session" || got.Result != OperationResultAccepted {
		t.Fatalf("lost destination or rewrote receipt: %+v", got)
	}
}

func TestReceiptDestinationAvailabilityFollowsStoredResource(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Conn.Exec(`INSERT INTO projects(id,work_dir) VALUES('p','/tmp/p')`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Conn.Exec(`INSERT INTO jobs(id,project_id) VALUES('session','p')`); err != nil {
		t.Fatal(err)
	}
	store := NewOperationResultStore(d.Conn)
	receipt := &OperationResult{TaskID: "card", OperationType: "card_command:discuss", Result: OperationResultAccepted, TargetSessionID: "session", TargetTaskID: "missing-task"}
	if err := store.CreateOperationResult(receipt); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetOperationResult("card", receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TargetSessionUnavailable || !got.TargetTaskUnavailable {
		t.Fatalf("availability=%+v", got)
	}
	if _, err := d.Conn.Exec(`DELETE FROM jobs WHERE id='session'`); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListOperationResults("card", 20)
	if err != nil {
		t.Fatal(err)
	}
	if !rows[0].TargetSessionUnavailable || rows[0].TargetSessionID != "session" {
		t.Fatalf("deleted session=%+v", rows[0])
	}
}
