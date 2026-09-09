package orchestrator

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
)

func TestOperationResultStorePersistsMetadataWithoutInput(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	store := NewOperationResultStore(d.Conn)
	want := &OperationResult{
		TaskID: "card-1", OperationType: "card_command:discuss",
		Result: OperationResultAccepted, ReasonCode: OperationReasonRequestAccepted,
		TargetRequestID: "request-1",
	}
	if err := store.CreateOperationResult(want); err != nil {
		t.Fatal(err)
	}
	if want.ID == "" || want.CreatedAt.IsZero() {
		t.Fatalf("server metadata not populated: %+v", want)
	}
	got, err := store.ListOperationResults("card-1", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].OperationType != want.OperationType || got[0].TargetRequestID != "request-1" {
		t.Fatalf("results = %+v", got)
	}
	var columns int
	if err := d.Conn.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('operation_results') WHERE name IN ('instruction','input','payload')`).Scan(&columns); err != nil {
		t.Fatal(err)
	}
	if columns != 0 {
		t.Fatalf("operation_results duplicates submitted input in %d columns", columns)
	}
}

func TestGCOperationResultsUsesIndependentThirtyDayAge(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	store := NewOperationResultStore(d.Conn)
	for _, r := range []*OperationResult{{TaskID: "card-1", OperationType: "go", Result: OperationResultRejected}, {TaskID: "card-1", OperationType: "go", Result: OperationResultStarted}} {
		if err := store.CreateOperationResult(r); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Conn.Exec(`UPDATE operation_results SET created_at = ? WHERE result = ?`, time.Now().UTC().Add(-31*24*time.Hour), OperationResultRejected); err != nil {
		t.Fatal(err)
	}
	n, err := GCOperationResults(d.Conn, 30*24*time.Hour, false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("deleted = %d, want 1", n)
	}
	got, err := store.ListOperationResults("card-1", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Result != OperationResultStarted {
		t.Fatalf("remaining = %+v", got)
	}
}

func TestOperationResultLifecycleOnlyEnrichesAcceptedRequest(t *testing.T) {
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
	card := &Task{ID: "card", Type: TaskTypeCard, ProjectID: "p", Status: TaskStatusParked, Card: &CardAttrs{}}
	if err := CreateTask(d.Conn, card); err != nil {
		t.Fatal(err)
	}
	req := &CardRequest{ID: "req", CardID: "card", CommandKey: "discuss", Status: CardRequestStatusLaunching, LauncherJobID: "launcher", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := CreateCardRequest(d.Conn, req); err != nil {
		t.Fatal(err)
	}
	store := NewOperationResultStore(d.Conn)
	rejected := &OperationResult{TaskID: "card", OperationType: "card_command:discuss", OperationLabel: "Discuss", Result: OperationResultRejected, ReasonCode: OperationReasonSlotOccupied, TargetRequestID: "req"}
	accepted := &OperationResult{TaskID: "card", OperationType: "card_command:discuss", OperationLabel: "Discuss", Result: OperationResultAccepted, ReasonCode: OperationReasonRequestAccepted, TargetRequestID: "req"}
	if err := store.CreateOperationResult(rejected); err != nil {
		t.Fatal(err)
	}
	if err := store.CreateOperationResult(accepted); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Conn.Exec(`UPDATE card_requests SET status='attached', target_kind='session', target_id='job-1' WHERE id='req'`); err != nil {
		t.Fatal(err)
	}
	gotRejected, _ := store.GetOperationResult("card", rejected.ID)
	gotAccepted, _ := store.GetOperationResult("card", accepted.ID)
	if gotRejected.Result != OperationResultRejected || gotRejected.CurrentPhase != "" {
		t.Fatalf("rejected result mutated: %+v", gotRejected)
	}
	if gotAccepted.Result != OperationResultAccepted || gotAccepted.CurrentPhase != OperationReasonTargetStarted || gotAccepted.TargetSessionID != "job-1" {
		t.Fatalf("accepted result not enriched: %+v", gotAccepted)
	}
	if _, err := d.Conn.Exec(`UPDATE card_requests SET status='failed' WHERE id='req'`); err != nil {
		t.Fatal(err)
	}
	gotAccepted, _ = store.GetOperationResult("card", accepted.ID)
	if gotAccepted.Result != OperationResultAccepted || gotAccepted.CurrentPhase != OperationReasonRequestFailed {
		t.Fatalf("failed accepted request rewrote receipt: %+v", gotAccepted)
	}
}
