package api

// Pins that a card's dispatched child's progress/awaiting/job-completion
// events reach the card's own SSE subscribers, not just its own action
// log. Both "fans out" and "does NOT fan out to a non-card parent" are
// pinned for each call site, plus the shared fanOutChildEventToParentCard/
// isCardTask decision point itself.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// postCommitFailTransactor runs fn against inner — so a body that (wrongly)
// broadcasts from INSIDE the transaction still does — then reports the
// transaction itself as failed. Distinct from alwaysFailTx (hub_broadcast_test.go),
// which never runs fn at all: this models a commit that fails only after its
// body already ran.
type postCommitFailTransactor struct {
	inner TxStore
}

func (t postCommitFailTransactor) WithinTx(fn func(TxStore) error) error {
	_ = fn(t.inner)
	return fmt.Errorf("simulated post-commit failure")
}

// ---- fanOutChildEventToParentCard / isCardTask: the single decision point ----

func TestIsCardTask(t *testing.T) {
	cases := []struct {
		name string
		task *orchestrator.Task
		want bool
	}{
		{"nil task", nil, false},
		{"card", &orchestrator.Task{Type: orchestrator.TaskTypeCard}, true},
		{"execution", &orchestrator.Task{Type: orchestrator.TaskTypeExecution}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCardTask(tc.task); got != tc.want {
				t.Errorf("isCardTask(%+v) = %v, want %v", tc.task, got, tc.want)
			}
		})
	}
}

func TestFanOutChildEventToParentCard_ReachesSubscriberOnlyWhenParentIsCard(t *testing.T) {
	card := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard}
	execParent := &orchestrator.Task{ID: "exec-parent-1", Type: orchestrator.TaskTypeExecution}
	childOfCard := &orchestrator.Task{ID: "child-1", ParentID: card.ID}
	childOfExec := &orchestrator.Task{ID: "child-2", ParentID: execParent.ID}
	rootTask := &orchestrator.Task{ID: "root-1"} // ParentID == ""

	tasks := &stubTaskStore{tasks: map[string]*orchestrator.Task{
		card.ID:       card,
		execParent.ID: execParent,
	}}

	t.Run("card parent receives the event", func(t *testing.T) {
		hub := NewTaskEventHub()
		ch := hub.Subscribe(context.Background(), card.ID)
		fanOutChildEventToParentCard(hub, tasks, childOfCard, TaskEvent{Kind: "child"})
		ev, ok := receiveEvent(t, ch, time.Second)
		if !ok || ev.Kind != "child" {
			t.Fatalf("expected a %q event on the card's channel, got ok=%v ev=%+v", "child", ok, ev)
		}
	})

	t.Run("execution parent does not receive the event", func(t *testing.T) {
		hub := NewTaskEventHub()
		ch := hub.Subscribe(context.Background(), execParent.ID)
		fanOutChildEventToParentCard(hub, tasks, childOfExec, TaskEvent{Kind: "child"})
		if _, ok := receiveEvent(t, ch, 50*time.Millisecond); ok {
			t.Fatal("execution parent must not receive a child fan-out event")
		}
	})

	t.Run("root task (no parent) is a no-op", func(t *testing.T) {
		hub := NewTaskEventHub()
		// Must not panic or block — there is no parent id to resolve at all.
		fanOutChildEventToParentCard(hub, tasks, rootTask, TaskEvent{Kind: "child"})
	})

	t.Run("nil hub is a no-op", func(t *testing.T) {
		fanOutChildEventToParentCard(nil, tasks, childOfCard, TaskEvent{Kind: "child"})
	})
}

// ---- ApplyAction: child executing→awaiting must fan out to a card parent ----

func TestApplyAction_FansOutToParentCard_WhenParentIsCard(t *testing.T) {
	card := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	child := &orchestrator.Task{
		ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: card.ID,
		Status: orchestrator.TaskStatusExecuting,
		Exec:   &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(`{}`)},
	}
	txStore := &recordingTxStore{task: child, tasks: map[string]*orchestrator.Task{child.ID: child, card.ID: card}}
	hub := NewTaskEventHub()
	selfCh := hub.Subscribe(context.Background(), child.ID)
	parentCh := hub.Subscribe(context.Background(), card.ID)

	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{tasks: map[string]*orchestrator.Task{child.ID: child, card.ID: card}},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {}}}},
		Hub:   hub,
	}

	if _, err := svc.ApplyAction(context.Background(), child.ID, ApplyActionRequest{Type: "ask"}); err != nil {
		t.Fatalf("ApplyAction(ask): %v", err)
	}

	if _, ok := receiveEvent(t, selfCh, time.Second); !ok {
		t.Fatal("child's own subscriber did not receive the existing self-broadcast (regression)")
	}
	ev, ok := receiveEvent(t, parentCh, time.Second)
	if !ok {
		t.Fatal("parent card did not receive the child→parent fan-out event")
	}
	if ev.Kind != "child" {
		t.Fatalf("event kind = %q, want %q", ev.Kind, "child")
	}
	payload, ok := ev.Payload.(map[string]any)
	if !ok || payload["child_task_id"] != child.ID {
		t.Fatalf("payload = %v, want child_task_id=%q", ev.Payload, child.ID)
	}
}

// TestApplyAction_DoesNotFanOut_WhenParentIsExecution pins that an
// execution parent (e.g. a supervisor/executor pair) sees no behavior
// change — only the child's own existing self-broadcast fires.
func TestApplyAction_DoesNotFanOut_WhenParentIsExecution(t *testing.T) {
	parent := &orchestrator.Task{ID: "exec-parent-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "impl"}}
	child := &orchestrator.Task{
		ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: parent.ID,
		Status: orchestrator.TaskStatusExecuting,
		Exec:   &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(`{}`)},
	}
	txStore := &recordingTxStore{task: child, tasks: map[string]*orchestrator.Task{child.ID: child, parent.ID: parent}}
	hub := NewTaskEventHub()
	parentCh := hub.Subscribe(context.Background(), parent.ID)

	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{tasks: map[string]*orchestrator.Task{child.ID: child, parent.ID: parent}},
		Tx:    recordingTransactor{store: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {}}}},
		Hub:   hub,
	}

	if _, err := svc.ApplyAction(context.Background(), child.ID, ApplyActionRequest{Type: "ask"}); err != nil {
		t.Fatalf("ApplyAction(ask): %v", err)
	}

	if _, ok := receiveEvent(t, parentCh, 50*time.Millisecond); ok {
		t.Fatal("execution parent must not receive a child fan-out event")
	}
}

// TestApplyAction_FanOut_NoBroadcastOnCommitFailure pins that the fan-out
// call sits AFTER WithinTx returns, not inside its closure: with
// postCommitFailTransactor the closure body runs (so a mutation moving the
// fan-out inside it would still fire), but WithinTx itself reports failure.
func TestApplyAction_FanOut_NoBroadcastOnCommitFailure(t *testing.T) {
	card := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	child := &orchestrator.Task{
		ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: card.ID,
		Status: orchestrator.TaskStatusExecuting,
		Exec:   &orchestrator.ExecAttrs{Behavior: "impl", Payload: []byte(`{}`)},
	}
	txStore := &recordingTxStore{task: child, tasks: map[string]*orchestrator.Task{child.ID: child, card.ID: card}}
	hub := NewTaskEventHub()
	selfCh := hub.Subscribe(context.Background(), child.ID)
	parentCh := hub.Subscribe(context.Background(), card.ID)

	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{tasks: map[string]*orchestrator.Task{child.ID: child, card.ID: card}},
		Tx:    postCommitFailTransactor{inner: txStore},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{TaskBehaviors: map[string]orchestrator.TaskBehavior{"impl": {}}}},
		Hub:   hub,
	}

	if _, err := svc.ApplyAction(context.Background(), child.ID, ApplyActionRequest{Type: "ask"}); err == nil {
		t.Fatal("ApplyAction(ask): expected error on commit failure, got nil")
	}

	if _, ok := receiveEvent(t, selfCh, 50*time.Millisecond); ok {
		t.Fatal("hub must not receive the self-broadcast event when commit fails")
	}
	if _, ok := receiveEvent(t, parentCh, 50*time.Millisecond); ok {
		t.Fatal("hub must not receive the fan-out event when commit fails")
	}
}

// ---- CompleteJob: child job completion/failure must fan out to a card parent ----

func TestCompleteJob_Success_FansOutToParentCard(t *testing.T) {
	card := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	child := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: card.ID, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "impl"}}
	job := &Job{ID: "job-1", TaskID: child.ID, ProjectID: "proj-1", Status: JobStatusRunning}

	hub := NewTaskEventHub()
	parentCh := hub.Subscribe(context.Background(), card.ID)

	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{tasks: map[string]*orchestrator.Task{child.ID: child, card.ID: card}},
		Jobs:  &stubJobStore{job: job},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Tx:    &stubTx{},
		Hub:   hub,
	}

	if _, err := svc.CompleteJob(context.Background(), job.ID, JobDoneRequest{ExitCode: 0}); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	ev, ok := receiveEvent(t, parentCh, time.Second)
	if !ok {
		t.Fatal("parent card did not receive the child job-completion fan-out event")
	}
	if ev.Kind != "child" {
		t.Fatalf("event kind = %q, want %q", ev.Kind, "child")
	}
}

func TestCompleteJob_Failure_FansOutToParentCard(t *testing.T) {
	card := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	child := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: card.ID, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "impl"}}
	job := &Job{ID: "job-1", TaskID: child.ID, ProjectID: "proj-1", Status: JobStatusRunning}

	hub := NewTaskEventHub()
	parentCh := hub.Subscribe(context.Background(), card.ID)

	svc := &TaskWorkflowService{
		Tasks:     &stubTaskStore{tasks: map[string]*orchestrator.Task{child.ID: child, card.ID: card}},
		Jobs:      &stubJobStore{job: job},
		Meta:      stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Lifecycle: &stubLifecycle{},
		Tx:        &stubTx{},
		Hub:       hub,
	}

	if _, err := svc.CompleteJob(context.Background(), job.ID, JobDoneRequest{ExitCode: 1}); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	ev, ok := receiveEvent(t, parentCh, time.Second)
	if !ok {
		t.Fatal("parent card did not receive the child job-failure fan-out event")
	}
	if ev.Kind != "child" {
		t.Fatalf("event kind = %q, want %q", ev.Kind, "child")
	}
}

// TestCompleteJob_Success_DoesNotFanOut_WhenParentIsExecution is CompleteJob's
// success-path negative case — TestApplyAction_DoesNotFanOut_WhenParentIsExecution
// only covers ApplyAction; the shared helper's own test covers CompleteJob's
// code path only indirectly.
func TestCompleteJob_Success_DoesNotFanOut_WhenParentIsExecution(t *testing.T) {
	parent := &orchestrator.Task{ID: "exec-parent-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "impl"}}
	child := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: parent.ID, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "impl"}}
	job := &Job{ID: "job-1", TaskID: child.ID, ProjectID: "proj-1", Status: JobStatusRunning}

	hub := NewTaskEventHub()
	parentCh := hub.Subscribe(context.Background(), parent.ID)

	svc := &TaskWorkflowService{
		Tasks: &stubTaskStore{tasks: map[string]*orchestrator.Task{child.ID: child, parent.ID: parent}},
		Jobs:  &stubJobStore{job: job},
		Meta:  stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Tx:    &stubTx{},
		Hub:   hub,
	}

	if _, err := svc.CompleteJob(context.Background(), job.ID, JobDoneRequest{ExitCode: 0}); err != nil {
		t.Fatalf("CompleteJob: %v", err)
	}

	if _, ok := receiveEvent(t, parentCh, 50*time.Millisecond); ok {
		t.Fatal("execution parent must not receive a child fan-out event")
	}
}

// TestCompleteJob_Failure_FanOut_NoBroadcastOnCommitFailure is
// TestApplyAction_FanOut_NoBroadcastOnCommitFailure's CompleteJob
// counterpart: the job_failed transaction and the fan-out sit on either
// side of the same WithinTx boundary.
func TestCompleteJob_Failure_FanOut_NoBroadcastOnCommitFailure(t *testing.T) {
	card := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	child := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: card.ID, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "impl"}}
	job := &Job{ID: "job-1", TaskID: child.ID, ProjectID: "proj-1", Status: JobStatusRunning}

	hub := NewTaskEventHub()
	selfCh := hub.Subscribe(context.Background(), child.ID)
	parentCh := hub.Subscribe(context.Background(), card.ID)

	svc := &TaskWorkflowService{
		Tasks:     &stubTaskStore{tasks: map[string]*orchestrator.Task{child.ID: child, card.ID: card}},
		Jobs:      &stubJobStore{job: job},
		Meta:      stubMetaStore{meta: &orchestrator.ProjectMeta{}},
		Lifecycle: &stubLifecycle{},
		Tx:        postCommitFailTransactor{inner: &stubTx{}},
		Hub:       hub,
	}

	if _, err := svc.CompleteJob(context.Background(), job.ID, JobDoneRequest{ExitCode: 1}); err == nil {
		t.Fatal("CompleteJob: expected error on commit failure, got nil")
	}

	if _, ok := receiveEvent(t, selfCh, 50*time.Millisecond); ok {
		t.Fatal("hub must not receive the self-broadcast event when commit fails")
	}
	if _, ok := receiveEvent(t, parentCh, 50*time.Millisecond); ok {
		t.Fatal("hub must not receive the fan-out event when commit fails")
	}
}

// ---- NotifyTask: progress/done_request/fail_request had NO Hub wiring at
// all before this PR (task_service.go's TaskAppService.Hub is new) ----

func TestNotifyTask_Progress_BroadcastsSelfAndFansOutToParentCard(t *testing.T) {
	card := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	child := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: card.ID, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "impl"}}

	hub := NewTaskEventHub()
	selfCh := hub.Subscribe(context.Background(), child.ID)
	parentCh := hub.Subscribe(context.Background(), card.ID)

	svc := &TaskAppService{
		Tasks:   &stubTaskStore{tasks: map[string]*orchestrator.Task{child.ID: child, card.ID: card}},
		Actions: &capturingActionStore{},
		Hub:     hub,
	}

	if err := svc.NotifyTask(context.Background(), child.ID, "", "", "", "halfway there", "", ""); err != nil {
		t.Fatalf("NotifyTask(progress): %v", err)
	}

	if _, ok := receiveEvent(t, selfCh, time.Second); !ok {
		t.Fatal("progress must broadcast to the reporting task's own subscribers (previously not wired at all)")
	}
	ev, ok := receiveEvent(t, parentCh, time.Second)
	if !ok {
		t.Fatal("progress must fan out to the parent card")
	}
	if ev.Kind != "child" {
		t.Fatalf("event kind = %q, want %q", ev.Kind, "child")
	}
}

func TestNotifyTask_FailRequest_BroadcastsSelfAndFansOutToParentCard(t *testing.T) {
	card := &orchestrator.Task{ID: "card-1", Type: orchestrator.TaskTypeCard, ProjectID: "proj-1", Status: orchestrator.TaskStatusWorking, Card: &orchestrator.CardAttrs{}}
	child := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: card.ID, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "impl"}}

	hub := NewTaskEventHub()
	selfCh := hub.Subscribe(context.Background(), child.ID)
	parentCh := hub.Subscribe(context.Background(), card.ID)

	svc := &TaskAppService{
		Tasks:    &stubTaskStore{tasks: map[string]*orchestrator.Task{child.ID: child, card.ID: card}},
		Jobs:     &stubJobStore{},
		Actions:  &capturingActionStore{},
		Workflow: &stubWorkflowService{},
		Hub:      hub,
	}

	if err := svc.NotifyTask(context.Background(), child.ID, "it broke", "", "", "", "", "boom"); err != nil {
		t.Fatalf("NotifyTask(fail): %v", err)
	}

	if _, ok := receiveEvent(t, selfCh, time.Second); !ok {
		t.Fatal("fail_request must broadcast to the reporting task's own subscribers (previously not wired at all)")
	}
	ev, ok := receiveEvent(t, parentCh, time.Second)
	if !ok {
		t.Fatal("fail_request must fan out to the parent card")
	}
	if ev.Kind != "child" {
		t.Fatalf("event kind = %q, want %q", ev.Kind, "child")
	}
}

// TestNotifyTask_Progress_RootTask_StillBroadcastsSelf pins that a root task
// (no parent at all) still gets its own self-broadcast — fanOutChildEventToParentCard's
// own no-parent case (already covered directly) is exercised incidentally
// here, not asserted on: there is no parent channel to observe.
func TestNotifyTask_Progress_RootTask_StillBroadcastsSelf(t *testing.T) {
	task := &orchestrator.Task{ID: "root-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "impl"}}

	hub := NewTaskEventHub()
	selfCh := hub.Subscribe(context.Background(), task.ID)

	svc := &TaskAppService{
		Tasks:   &stubTaskStore{task: task},
		Actions: &capturingActionStore{},
		Hub:     hub,
	}

	if err := svc.NotifyTask(context.Background(), task.ID, "", "", "", "progressing", "", ""); err != nil {
		t.Fatalf("NotifyTask(progress): %v", err)
	}

	if _, ok := receiveEvent(t, selfCh, time.Second); !ok {
		t.Fatal("progress must still broadcast to the reporting task's own subscribers")
	}
}

// TestNotifyTask_Progress_DoesNotFanOut_WhenParentIsExecution is
// broadcastNotifyAction's own execution-parent negative case — the shared
// fanOutChildEventToParentCard unit test covers the predicate itself, but
// not that this specific call site actually passes it through untouched.
func TestNotifyTask_Progress_DoesNotFanOut_WhenParentIsExecution(t *testing.T) {
	parent := &orchestrator.Task{ID: "exec-parent-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "impl"}}
	child := &orchestrator.Task{ID: "child-1", Type: orchestrator.TaskTypeExecution, ProjectID: "proj-1", ParentID: parent.ID, Status: orchestrator.TaskStatusExecuting, Exec: &orchestrator.ExecAttrs{Behavior: "impl"}}

	hub := NewTaskEventHub()
	parentCh := hub.Subscribe(context.Background(), parent.ID)

	svc := &TaskAppService{
		Tasks:   &stubTaskStore{tasks: map[string]*orchestrator.Task{child.ID: child, parent.ID: parent}},
		Actions: &capturingActionStore{},
		Hub:     hub,
	}

	if err := svc.NotifyTask(context.Background(), child.ID, "", "", "", "progressing", "", ""); err != nil {
		t.Fatalf("NotifyTask(progress): %v", err)
	}

	if _, ok := receiveEvent(t, parentCh, 50*time.Millisecond); ok {
		t.Fatal("execution parent must not receive a child fan-out event")
	}
}
