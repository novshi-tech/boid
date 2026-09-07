package api

// TestWebHandlerTaskList_ActivityState_QueryCountDoesNotScaleWithRowCount
// pins §5.5's "一覧の読みは一括取得とし、行ごとの追加問い合わせを増やさない"
// (docs/plans/card-next-step-and-timeline.md): the list's new per-card
// activity state (card_requests + dispatched-child status) must add a FIXED
// number of queries per page, not one per card row.

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// countingDBTX wraps a real db.DBTX, counting every Query/QueryRow/Exec call
// through it — the N+1 regression guard's instrument.
type countingDBTX struct {
	inner db.DBTX
	n     *int
}

func (c countingDBTX) Exec(query string, args ...any) (sql.Result, error) {
	*c.n++
	return c.inner.Exec(query, args...)
}

func (c countingDBTX) Query(query string, args ...any) (*sql.Rows, error) {
	*c.n++
	return c.inner.Query(query, args...)
}

func (c countingDBTX) QueryRow(query string, args ...any) *sql.Row {
	*c.n++
	return c.inner.QueryRow(query, args...)
}

// seedActivityN1Fixture creates n cards under projectID, each with one
// dispatched work child pointing at a real (pending) task, plus a queued
// card command on every other card — a realistic mix that exercises both
// batched lookups (TaskStatusesByIDs, ActiveCardRequestsByCardIDs), not just
// their zero-row fast paths.
func seedActivityN1Fixture(t *testing.T, conn *sql.DB, projectID string, n int) {
	t.Helper()
	if err := orchestrator.CreateProject(conn, &orchestrator.Project{ID: projectID, WorkDir: "/tmp/" + projectID}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	for i := 0; i < n; i++ {
		cardID := fmt.Sprintf("card-%03d", i)
		childTaskID := fmt.Sprintf("child-%03d", i)

		card := &orchestrator.Task{ID: cardID, ProjectID: projectID, Type: orchestrator.TaskTypeCard, Card: &orchestrator.CardAttrs{}}
		if err := orchestrator.CreateTask(conn, card); err != nil {
			t.Fatalf("create card %q: %v", cardID, err)
		}
		child := &orchestrator.Task{ID: childTaskID, ProjectID: projectID, Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{}}
		if err := orchestrator.CreateTask(conn, child); err != nil {
			t.Fatalf("create child task %q: %v", childTaskID, err)
		}
		detail := fmt.Sprintf(`{"children":[{"id":"c1","status":"dispatched","task_ref":%q}]}`, childTaskID)
		if err := orchestrator.UpsertTaskTriage(conn, &orchestrator.CardAttrs{TaskID: cardID, Detail: []byte(detail)}); err != nil {
			t.Fatalf("upsert task triage for %q: %v", cardID, err)
		}
		if i%2 == 0 {
			req := &orchestrator.CardRequest{CardID: cardID, CommandKey: "sweep", Status: orchestrator.CardRequestStatusQueued}
			if err := orchestrator.CreateCardRequest(conn, req); err != nil {
				t.Fatalf("create card request for %q: %v", cardID, err)
			}
		}
	}
}

// runActivityN1TaskList builds a fresh in-memory DB, seeds n cards, issues
// one GET / through WebHandler.TaskList, and returns how many DB
// statements (Query/QueryRow/Exec) that single request issued.
func runActivityN1TaskList(t *testing.T, n int) int {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	seedActivityN1Fixture(t, d.Conn, "proj-1", n)

	var count int
	wrapped := countingDBTX{inner: d.Conn, n: &count}
	repo := orchestrator.NewTaskRepository(wrapped)

	h := &WebHandler{
		Service: &WebAppService{
			Tasks:    repo,
			Projects: &stubProjectRepository{},
			Meta:     stubMetaStore{},
		},
		TaskTriage:   repo,
		CardActivity: repo,
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	count = 0
	h.TaskList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("TaskList status = %d, body = %s", rec.Code, rec.Body.String())
	}
	return count
}

func TestWebHandlerTaskList_ActivityState_QueryCountDoesNotScaleWithRowCount(t *testing.T) {
	small := runActivityN1TaskList(t, 3)
	large := runActivityN1TaskList(t, 30)
	if small != large {
		t.Fatalf("query count scales with row count: 3 cards = %d queries, 30 cards = %d queries (want equal — one page's activity state must be one batch, not one query per row)", small, large)
	}
	// Pins the actual fixed count (ListTasks, ListTaskTriageByTaskIDs,
	// ActiveCardRequestsByCardIDs, TaskStatusesByIDs), not just "equal" — a
	// change that adds a query but keeps it constant per page would pass the
	// equality check above but should still be caught.
	if small != 4 {
		t.Fatalf("query count = %d, want 4 (ListTasks + ListTaskTriageByTaskIDs + ActiveCardRequestsByCardIDs + TaskStatusesByIDs)", small)
	}
}

// End-to-end pin (real DB, real render): the batched activity lookups must
// actually reach the rendered row, not just avoid N+1 while returning
// nothing useful.
func TestWebHandlerTaskList_RendersActivityBadgesFromRealDB(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	card := &orchestrator.Task{ID: "card-1", ProjectID: "proj-1", Type: orchestrator.TaskTypeCard, Card: &orchestrator.CardAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, card); err != nil {
		t.Fatalf("create card: %v", err)
	}
	child := &orchestrator.Task{ID: "child-1", ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, child); err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := orchestrator.UpsertTaskTriage(d.Conn, &orchestrator.CardAttrs{
		TaskID: "card-1",
		Detail: []byte(`{"children":[{"id":"c1","status":"dispatched","task_ref":"child-1"}]}`),
	}); err != nil {
		t.Fatalf("upsert task triage: %v", err)
	}
	cmdReq := &orchestrator.CardRequest{
		CardID: "card-1", CommandKey: "discuss", Status: orchestrator.CardRequestStatusLaunching,
		LauncherJobID: "job-1", Launched: orchestrator.CardRequestDefinition{Label: "Discuss"},
	}
	if err := orchestrator.CreateCardRequest(d.Conn, cmdReq); err != nil {
		t.Fatalf("create card request: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, cmdReq.ID, orchestrator.CardRequestTargetKindTask, "child-1"); err != nil {
		t.Fatalf("attach card request: %v", err)
	}

	repo := orchestrator.NewTaskRepository(d.Conn)
	h := &WebHandler{
		Service: &WebAppService{
			Tasks:    repo,
			Projects: &stubProjectRepository{},
			Meta:     stubMetaStore{},
		},
		TaskTriage:   repo,
		CardActivity: repo,
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.TaskList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("TaskList status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// The dispatched child's REAL task is "executing" — the work badge must
	// say "Running", proving the real task row (not the JSON "dispatched"
	// status) drove the label.
	if !strings.Contains(body, "list-row-activity-work") || !strings.Contains(body, ">Running<") {
		t.Errorf("expected the work activity badge with text %q in the rendered page, got:\n%s", "Running", body)
	}
	if !strings.Contains(body, "Discuss: Running") {
		t.Errorf("expected the command activity badge with text %q in the rendered page, got:\n%s", "Discuss: Running", body)
	}
}

// A command attached to a task target whose real task is awaiting an answer
// must render "Needs input" for the command axis too, not "Running" — an
// awaiting dialogue task must never look identical to a live one in the
// list, the same rule already applied to the work-child axis. This exercises
// the shared TaskStatusesByIDs batch: the command's TargetID is folded into
// the same query as the dispatched child's TaskRef, at zero extra queries.
func TestWebHandlerTaskList_CommandAttachedToAwaitingTask_RendersNeedsInput(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp/proj-1"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	card := &orchestrator.Task{ID: "card-1", ProjectID: "proj-1", Type: orchestrator.TaskTypeCard, Card: &orchestrator.CardAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, card); err != nil {
		t.Fatalf("create card: %v", err)
	}
	// The command's target task, awaiting an answer — no work child on this
	// card, so only the command axis is exercised.
	dialogueTask := &orchestrator.Task{ID: "dialogue-1", ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusAwaiting, Exec: &orchestrator.ExecAttrs{}}
	if err := orchestrator.CreateTask(d.Conn, dialogueTask); err != nil {
		t.Fatalf("create dialogue task: %v", err)
	}
	cmdReq := &orchestrator.CardRequest{
		CardID: "card-1", CommandKey: "discuss", Status: orchestrator.CardRequestStatusLaunching,
		LauncherJobID: "job-1", Launched: orchestrator.CardRequestDefinition{Label: "Discuss"},
	}
	if err := orchestrator.CreateCardRequest(d.Conn, cmdReq); err != nil {
		t.Fatalf("create card request: %v", err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, cmdReq.ID, orchestrator.CardRequestTargetKindTask, "dialogue-1"); err != nil {
		t.Fatalf("attach card request: %v", err)
	}

	var count int
	wrapped := countingDBTX{inner: d.Conn, n: &count}
	repo := orchestrator.NewTaskRepository(wrapped)
	h := &WebHandler{
		Service: &WebAppService{
			Tasks:    repo,
			Projects: &stubProjectRepository{},
			Meta:     stubMetaStore{},
		},
		TaskTriage:   repo,
		CardActivity: repo,
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.TaskList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("TaskList status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Discuss: Needs input") {
		t.Errorf("expected the command badge to read %q (real task is awaiting), got:\n%s", "Discuss: Needs input", body)
	}
	if strings.Contains(body, "Discuss: Running") {
		t.Errorf("command badge must not say Running while its target task is awaiting an answer, got:\n%s", body)
	}
	if count != 4 {
		t.Errorf("query count = %d, want 4 (folding the command's task target into the same TaskStatusesByIDs batch must not add a query)", count)
	}
}
