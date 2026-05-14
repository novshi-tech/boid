package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type capturingNotifier struct {
	event notify.Event
	err   error
}

func (n *capturingNotifier) Notify(_ context.Context, ev notify.Event) error {
	n.event = ev
	return n.err
}

func TestNotifyTask_InteractiveRunningJobSetsJobID(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
	}
	jobs := []*Job{
		{ID: "j1", TaskID: "t1", Status: JobStatusCompleted, Interactive: false},
		{ID: "j2", TaskID: "t1", Status: JobStatusRunning, Interactive: true},
	}
	notifier := &capturingNotifier{}
	svc := &TaskAppService{
		Tasks:  &stubTaskStore{task: task},
		Jobs:   &stubJobStore{jobsByTask: map[string][]*Job{task.ID: jobs}},
		Notify: notifier,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "hello", "", "", "", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if notifier.event.JobID != "j2" {
		t.Errorf("JobID = %q, want %q", notifier.event.JobID, "j2")
	}
}

func TestNotifyTask_NoInteractiveRunningJob_JobIDEmpty(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
	}
	jobs := []*Job{
		{ID: "j1", TaskID: "t1", Status: JobStatusCompleted, Interactive: true},
		{ID: "j2", TaskID: "t1", Status: JobStatusRunning, Interactive: false},
	}
	notifier := &capturingNotifier{}
	svc := &TaskAppService{
		Tasks:  &stubTaskStore{task: task},
		Jobs:   &stubJobStore{jobsByTask: map[string][]*Job{task.ID: jobs}},
		Notify: notifier,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "hello", "", "", "", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if notifier.event.JobID != "" {
		t.Errorf("JobID = %q, want empty", notifier.event.JobID)
	}
}

func TestNotifyTask_AskMode_TransitionsToAwaiting(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
	}
	notifier := &capturingNotifier{}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Notify:   notifier,
		Workflow: workflow,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "Plan ready", "Approve?", "q-1", "", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if notifier.event.Message != "Plan ready" {
		t.Errorf("message = %q, want Plan ready", notifier.event.Message)
	}
	if workflow.appliedType != "ask" {
		t.Errorf("applied action type = %q, want ask", workflow.appliedType)
	}
}

// notify --ask must terminate any running hook jobs for the task so the
// interactive claude session that called notify actually exits. Without this
// the PTY-backed runtime keeps running even though the task is now awaiting.
func TestNotifyTask_AskMode_CompletesRunningJobs(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
	}
	jobs := []*Job{
		{ID: "j-completed", TaskID: "t1", Status: JobStatusCompleted},
		{ID: "j-running", TaskID: "t1", Status: JobStatusRunning, Interactive: true},
		{ID: "j-failed", TaskID: "t1", Status: JobStatusFailed},
	}
	notifier := &capturingNotifier{}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Jobs:     &stubJobStore{jobsByTask: map[string][]*Job{task.ID: jobs}},
		Notify:   notifier,
		Workflow: workflow,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "Need decision", "Continue?", "q-1", "", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if workflow.appliedType != "ask" {
		t.Fatalf("applied action type = %q, want ask", workflow.appliedType)
	}
	if len(workflow.completedJobs) != 1 {
		t.Fatalf("completed jobs = %d, want 1 (only the running one)", len(workflow.completedJobs))
	}
	if workflow.completedJobs[0].JobID != "j-running" {
		t.Errorf("completed jobs[0].JobID = %q, want j-running (terminal-state jobs must be skipped)", workflow.completedJobs[0].JobID)
	}
	if workflow.completedJobs[0].ExitCode != 0 {
		t.Errorf("completed jobs[0].ExitCode = %d, want 0 (Q&A pause must not look like a job_failed)", workflow.completedJobs[0].ExitCode)
	}
}

// Progress mode (notify --progress) never terminates jobs — it just records a
// timeline entry — so the running hook job is left alone.
func TestNotifyTask_ProgressMode_LeavesRunningJobsAlone(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
	}
	jobs := []*Job{
		{ID: "j-running", TaskID: "t1", Status: JobStatusRunning, Interactive: true},
	}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Actions:  stubActionStore{},
		Jobs:     &stubJobStore{jobsByTask: map[string][]*Job{task.ID: jobs}},
		Workflow: workflow,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "", "", "", "", "stage 2 done"); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if len(workflow.completedJobs) != 0 {
		t.Errorf("completed jobs = %v, want none (progress mode does not pause)", workflow.completedJobs)
	}
}

func TestNotifyTask_AskMode_SetsQuestionPageURLPath(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
	}
	notifier := &capturingNotifier{}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Notify:   notifier,
		Workflow: workflow,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "Plan ready", "Approve?", "q-1", "", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	want := "/tasks/t1/questions/q-1"
	if notifier.event.URLPath != want {
		t.Errorf("URLPath = %q, want %q", notifier.event.URLPath, want)
	}
	// JobID lookup should be skipped in ask mode (deep-link to Q&A page instead).
	if notifier.event.JobID != "" {
		t.Errorf("JobID = %q, want empty in ask mode", notifier.event.JobID)
	}
}

func TestNotifyTask_AskMode_GeneratesQuestionIDForURL(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
	}
	notifier := &capturingNotifier{}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Notify:   notifier,
		Workflow: workflow,
	}

	// Caller omits questionID; service must generate one and reflect it in the URL.
	if err := svc.NotifyTask(context.Background(), "t1", "msg", "Approve?", "", "", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	prefix := "/tasks/t1/questions/"
	if len(notifier.event.URLPath) <= len(prefix) || notifier.event.URLPath[:len(prefix)] != prefix {
		t.Errorf("URLPath = %q, want prefix %q with auto-generated id", notifier.event.URLPath, prefix)
	}
}

// capturingActionStore captures the most recently created action for assertions.
type capturingActionStore struct {
	createdAction *orchestrator.Action
}

func (s *capturingActionStore) CreateAction(action *orchestrator.Action) error {
	s.createdAction = action
	return nil
}

func (s *capturingActionStore) ListActionsByTask(taskID string) ([]*orchestrator.Action, error) {
	return nil, nil
}

func TestNotifyTask_ProgressMode_CreatesActionNoHook(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "dev",
	}
	notifier := &capturingNotifier{}
	actions := &capturingActionStore{}
	svc := &TaskAppService{
		Tasks:   &stubTaskStore{task: task},
		Actions: actions,
		Notify:  notifier,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "", "", "", "", "step 2 done"); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}

	// Hook (external notifier) must NOT be called.
	if (notifier.event != notify.Event{}) {
		t.Errorf("external notifier should not be called in progress mode, got event %+v", notifier.event)
	}

	// Action must be created with correct fields.
	if actions.createdAction == nil {
		t.Fatal("expected a progress Action to be created")
	}
	a := actions.createdAction
	if a.Type != "progress" {
		t.Errorf("action.Type = %q, want progress", a.Type)
	}
	if a.FromStatus != orchestrator.TaskStatusExecuting {
		t.Errorf("action.FromStatus = %q, want %q", a.FromStatus, orchestrator.TaskStatusExecuting)
	}
	if a.ToStatus != orchestrator.TaskStatusExecuting {
		t.Errorf("action.ToStatus = %q, want %q", a.ToStatus, orchestrator.TaskStatusExecuting)
	}
	// Task status unchanged.
	if task.Status != orchestrator.TaskStatusExecuting {
		t.Errorf("task.Status changed to %q, should remain executing", task.Status)
	}
}

func TestNotifyTask_ProgressMode_NoNotifierRequired(t *testing.T) {
	task := &orchestrator.Task{
		ID:     "t1",
		Status: orchestrator.TaskStatusExecuting,
	}
	actions := &capturingActionStore{}
	svc := &TaskAppService{
		Tasks:   &stubTaskStore{task: task},
		Actions: actions,
		// Notify is nil — progress should still work
	}

	err := svc.NotifyTask(context.Background(), "t1", "", "", "", "", "midway")
	if err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if actions.createdAction == nil {
		t.Fatal("expected a progress Action to be created")
	}
}

func TestNotifyTask_ProgressAndAskMutuallyExclusive(t *testing.T) {
	task := &orchestrator.Task{
		ID:     "t1",
		Status: orchestrator.TaskStatusExecuting,
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{task: task},
	}

	err := svc.NotifyTask(context.Background(), "t1", "msg", "question?", "", "", "progress text")
	if err == nil {
		t.Fatal("expected error when both ask and progress are set")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Errorf("expected 400 StatusError, got %v", err)
	}
}

func TestAnswerTask_TransitionsToExecuting(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusAwaiting,
		Behavior:  "dev",
	}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Workflow: workflow,
	}

	if err := svc.AnswerTask(context.Background(), "t1", "q-1", "yes"); err != nil {
		t.Fatalf("AnswerTask: %v", err)
	}
	if workflow.appliedType != "answer" {
		t.Errorf("applied action type = %q, want answer", workflow.appliedType)
	}
}

func TestAnswerTask_NotAwaiting_ReturnsConflict(t *testing.T) {
	task := &orchestrator.Task{
		ID:       "t1",
		Status:   orchestrator.TaskStatusExecuting,
		Behavior: "dev",
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{task: task},
	}

	err := svc.AnswerTask(context.Background(), "t1", "q-1", "yes")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != 409 {
		t.Errorf("expected StatusError 409, got %v", err)
	}
}
