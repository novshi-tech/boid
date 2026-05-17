package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/novshi-tech/boid/internal/notify"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

type capturingNotifier struct {
	event  notify.Event
	called int
	err    error
}

func (n *capturingNotifier) Notify(_ context.Context, ev notify.Event) error {
	n.event = ev
	n.called++
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

	if err := svc.NotifyTask(context.Background(), "t1", "hello", "", "", "", "", "", ""); err != nil {
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

	if err := svc.NotifyTask(context.Background(), "t1", "hello", "", "", "", "", "", ""); err != nil {
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

	if err := svc.NotifyTask(context.Background(), "t1", "Plan ready", "Approve?", "q-1", "", "", "", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if notifier.event.Message != "Plan ready" {
		t.Errorf("message = %q, want Plan ready", notifier.event.Message)
	}
	if workflow.appliedType != "ask" {
		t.Errorf("applied action type = %q, want ask", workflow.appliedType)
	}
}

// notify --ask must signal the agent (claude) of each running hook job so
// the interactive session that called notify actually pauses. Without this
// the PTY-backed runtime keeps running forever even though the task is now
// awaiting. The signal is routed as a StopAgent call (SIGUSR1 to the
// runtime process group, handled by run-agent.py) rather than CompleteJob:
// CompleteJob would release the broker token and reject the bash EXIT
// trap's `boid job done --output-file payload_patch.json` as "invalid
// token", silently dropping the agent's session id and breaking the next
// hook's resume.
func TestNotifyTask_AskMode_StopsAgentForRunningJobs(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
	}
	jobs := []*Job{
		{ID: "j-completed", TaskID: "t1", Status: JobStatusCompleted, RuntimeID: "rt-completed"},
		{ID: "j-running", TaskID: "t1", Status: JobStatusRunning, Interactive: true, RuntimeID: "rt-running"},
		{ID: "j-failed", TaskID: "t1", Status: JobStatusFailed, RuntimeID: "rt-failed"},
		{ID: "j-no-runtime", TaskID: "t1", Status: JobStatusRunning, Interactive: true, RuntimeID: ""},
	}
	notifier := &capturingNotifier{}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Jobs:     &stubJobStore{jobsByTask: map[string][]*Job{task.ID: jobs}},
		Notify:   notifier,
		Workflow: workflow,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "Need decision", "Continue?", "q-1", "", "", "", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if workflow.appliedType != "ask" {
		t.Fatalf("applied action type = %q, want ask", workflow.appliedType)
	}
	// CompleteJob must NOT be called preemptively — that would invalidate the
	// broker token and lose the EXIT trap's payload_patch.
	if len(workflow.completedJobs) != 0 {
		t.Fatalf("completed jobs = %d, want 0 (CompleteJob must not preempt the EXIT trap path)", len(workflow.completedJobs))
	}
	// Only the running job with a RuntimeID should receive StopAgent.
	if len(workflow.stoppedAgentRuntimes) != 1 {
		t.Fatalf("stopped agent runtimes = %v, want exactly [rt-running]", workflow.stoppedAgentRuntimes)
	}
	if workflow.stoppedAgentRuntimes[0] != "rt-running" {
		t.Errorf("stopped agent runtimes[0] = %q, want rt-running", workflow.stoppedAgentRuntimes[0])
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

	if err := svc.NotifyTask(context.Background(), "t1", "", "", "", "", "stage 2 done", "", ""); err != nil {
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

	if err := svc.NotifyTask(context.Background(), "t1", "Plan ready", "Approve?", "q-1", "", "", "", ""); err != nil {
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
	if err := svc.NotifyTask(context.Background(), "t1", "msg", "Approve?", "", "", "", "", ""); err != nil {
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

// Per-mode regression tests for notify --done / --fail: they MUST NOT call
// ApplyAction (which would synchronously transition the task and SIGTERM the
// runtime), and MUST instead record a non-transitioning done_request /
// fail_request action so the dispatch loop's auto-advance picks up the intent
// once the runtime exits cleanly. See investigation of job 69b1b0a4 (Phase
// 2.c race: notify --done → ApplyAction(done) → finalizeTerminal →
// CleanupTaskWindow SIGTERM → bash dies before EXIT trap can call boid job
// done → watchRuntime marks job failed).
func TestNotifyTask_DoneMode_RecordsDoneRequestActionNotApplyAction(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
	}
	jobs := []*Job{
		{ID: "j-running", TaskID: "t1", Status: JobStatusRunning, Interactive: true, RuntimeID: "rt-running"},
	}
	notifier := &capturingNotifier{}
	workflow := &stubWorkflowService{}
	actions := &capturingActionStore{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Jobs:     &stubJobStore{jobsByTask: map[string][]*Job{task.ID: jobs}},
		Actions:  actions,
		Notify:   notifier,
		Workflow: workflow,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "headline", "", "", "", "", "PR #439 merged", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}

	if workflow.appliedType != "" {
		t.Errorf("ApplyAction must NOT be called for --done (got type=%q); transition must happen via auto-advance after the runtime exits", workflow.appliedType)
	}
	if actions.createdAction == nil {
		t.Fatal("expected done_request action to be recorded")
	}
	a := actions.createdAction
	if a.Type != "done_request" {
		t.Errorf("action.Type = %q, want done_request", a.Type)
	}
	if a.FromStatus != orchestrator.TaskStatusExecuting || a.ToStatus != orchestrator.TaskStatusExecuting {
		t.Errorf("action must be non-transitioning, got %s → %s", a.FromStatus, a.ToStatus)
	}
	if string(a.Payload) != `{"message":"PR #439 merged"}` {
		t.Errorf("action.Payload = %s, want {\"message\":\"PR #439 merged\"}", a.Payload)
	}
	// SIGUSR1 path (graceful claude shutdown) must still fire so the runtime
	// can exit and the bash EXIT trap can call boid job done. This is the
	// signal the auto-advance is waiting for (lifecycle.executed).
	if len(workflow.stoppedAgentRuntimes) != 1 || workflow.stoppedAgentRuntimes[0] != "rt-running" {
		t.Errorf("stopped agent runtimes = %v, want [rt-running]", workflow.stoppedAgentRuntimes)
	}
}

func TestNotifyTask_FailMode_RecordsFailRequestActionNotApplyAction(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Title:     "my task",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
	}
	jobs := []*Job{
		{ID: "j-running", TaskID: "t1", Status: JobStatusRunning, Interactive: true, RuntimeID: "rt-running"},
	}
	notifier := &capturingNotifier{}
	workflow := &stubWorkflowService{}
	actions := &capturingActionStore{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Jobs:     &stubJobStore{jobsByTask: map[string][]*Job{task.ID: jobs}},
		Actions:  actions,
		Notify:   notifier,
		Workflow: workflow,
	}

	if err := svc.NotifyTask(context.Background(), "t1", "headline", "", "", "", "", "", "tests broken"); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}

	if workflow.appliedType != "" {
		t.Errorf("ApplyAction must NOT be called for --fail (got type=%q)", workflow.appliedType)
	}
	if actions.createdAction == nil {
		t.Fatal("expected fail_request action to be recorded")
	}
	a := actions.createdAction
	if a.Type != "fail_request" {
		t.Errorf("action.Type = %q, want fail_request", a.Type)
	}
	if a.FromStatus != orchestrator.TaskStatusExecuting || a.ToStatus != orchestrator.TaskStatusExecuting {
		t.Errorf("action must be non-transitioning, got %s → %s", a.FromStatus, a.ToStatus)
	}
	if string(a.Payload) != `{"message":"tests broken"}` {
		t.Errorf("action.Payload = %s, want {\"message\":\"tests broken\"}", a.Payload)
	}
	if len(workflow.stoppedAgentRuntimes) != 1 || workflow.stoppedAgentRuntimes[0] != "rt-running" {
		t.Errorf("stopped agent runtimes = %v, want [rt-running]", workflow.stoppedAgentRuntimes)
	}
}

// notify --done from a non-executing task must error out cleanly rather than
// silently record an orphan done_request. The previous design routed
// awaiting → done through `notify --done` from the parent supervisor, but
// that path now belongs to `action send --type done`; agents only call
// notify --done from executing.
func TestNotifyTask_DoneMode_NonExecutingTask_Errors(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "t1",
		ProjectID: "proj-1",
		Status:    orchestrator.TaskStatusAwaiting,
	}
	notifier := &capturingNotifier{}
	actions := &capturingActionStore{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Actions:  actions,
		Notify:   notifier,
		Workflow: &stubWorkflowService{},
	}

	err := svc.NotifyTask(context.Background(), "t1", "headline", "", "", "", "", "done msg", "")
	if err == nil {
		t.Fatal("expected error when calling --done on non-executing task")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusConflict {
		t.Fatalf("expected StatusConflict, got %v", err)
	}
	if actions.createdAction != nil {
		t.Errorf("no action should be recorded on error, got %+v", actions.createdAction)
	}
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

	if err := svc.NotifyTask(context.Background(), "t1", "", "", "", "", "step 2 done", "", ""); err != nil {
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

	err := svc.NotifyTask(context.Background(), "t1", "", "", "", "", "midway", "", "")
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

	err := svc.NotifyTask(context.Background(), "t1", "msg", "question?", "", "", "progress text", "", "")
	if err == nil {
		t.Fatal("expected error when both ask and progress are set")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusBadRequest {
		t.Errorf("expected 400 StatusError, got %v", err)
	}
}

// Lifecycle-accountability gate: child tasks (parent_id != "") never fire the
// user-facing notify hook. The supervisor is responsible for noticing the
// awaiting child via its monitoring loop.
func TestNotifyTask_ChildTaskAsk_SkipsHookButTransitionsToAwaiting(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "child-1",
		ParentID:  "supervisor-1",
		ProjectID: "proj-1",
		Title:     "child work",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
	}
	notifier := &capturingNotifier{}
	workflow := &stubWorkflowService{}
	svc := &TaskAppService{
		Tasks:    &stubTaskStore{task: task},
		Notify:   notifier,
		Workflow: workflow,
	}

	if err := svc.NotifyTask(context.Background(), "child-1", "done", "done_request: subtree finished", "q-1", "", "", "", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if notifier.called != 0 {
		t.Errorf("notifier called %d times for child task, want 0 (parent_id gate)", notifier.called)
	}
	if workflow.appliedType != "ask" {
		t.Errorf("applied action type = %q, want ask (state transition must still happen)", workflow.appliedType)
	}
}

func TestNotifyTask_ChildTaskFYI_SkipsHookSilently(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "child-1",
		ParentID:  "supervisor-1",
		ProjectID: "proj-1",
		Title:     "child work",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
	}
	notifier := &capturingNotifier{}
	svc := &TaskAppService{
		Tasks:  &stubTaskStore{task: task},
		Notify: notifier,
	}

	if err := svc.NotifyTask(context.Background(), "child-1", "milestone", "", "", "", "", "", ""); err != nil {
		t.Fatalf("NotifyTask: %v", err)
	}
	if notifier.called != 0 {
		t.Errorf("notifier called %d times for child task FYI, want 0", notifier.called)
	}
}

// Without a notifier, a child task FYI must NOT error out — the hook is skipped
// anyway. (Root task FYI without a notifier still errors; see existing tests.)
func TestNotifyTask_ChildTaskFYI_NoNotifierIsFine(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "child-1",
		ParentID:  "supervisor-1",
		ProjectID: "proj-1",
		Title:     "child work",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "executor",
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{task: task},
		// Notify intentionally nil
	}

	if err := svc.NotifyTask(context.Background(), "child-1", "milestone", "", "", "", "", "", ""); err != nil {
		t.Errorf("NotifyTask returned error for child FYI without notifier: %v", err)
	}
}

func TestNotifyTask_RootTaskFYI_NoNotifierStillErrors(t *testing.T) {
	task := &orchestrator.Task{
		ID:        "root-1",
		ParentID:  "",
		ProjectID: "proj-1",
		Title:     "root work",
		Status:    orchestrator.TaskStatusExecuting,
		Behavior:  "supervisor",
	}
	svc := &TaskAppService{
		Tasks: &stubTaskStore{task: task},
		// Notify intentionally nil
	}

	err := svc.NotifyTask(context.Background(), "root-1", "milestone", "", "", "", "", "", "")
	if err == nil {
		t.Fatal("expected error: root FYI without notifier should fail")
	}
	se, ok := err.(*StatusError)
	if !ok || se.Code != http.StatusNotImplemented {
		t.Errorf("expected 501 StatusError, got %v", err)
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
