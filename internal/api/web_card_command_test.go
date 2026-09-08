package api

// The card detail page's shared instruction textarea and project.yaml
// -declared card_commands buttons. Wired against a real (in-memory SQLite)
// DB, sharing one connection between the page's own read model and
// RunCardCommandAsHuman's occupancy check/dispatch, the same way
// web_card_timeline_test.go's fixtures already do.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// cardCommandWebService extends dbBackedWebService with the two card-command
// methods PostCardCommand/CardCommandSection need, delegating to a real
// *TaskWorkflowService that shares the SAME db connection as the embedded
// dbBackedWebService's repo — a command's occupancy check/dispatch and the
// page's own pinned-item read model must see the same rows.
type cardCommandWebService struct {
	dbBackedWebService
	workflow *TaskWorkflowService
}

func (s cardCommandWebService) RunCardCommandAsHuman(ctx context.Context, cardID, commandKey, instruction string) (*RunCardCommandResult, error) {
	return s.workflow.RunCardCommandAsHuman(ctx, cardID, commandKey, instruction)
}

func (s cardCommandWebService) CardCommandOptionsForProject(ctx context.Context, projectID string) []CardCommandOption {
	return s.workflow.CardCommandOptionsForProject(ctx, projectID)
}

// newCardCommandWebTestHandler builds a WebHandler wired end to end for the
// command section tests: real DB, a real TaskWorkflowService backing
// RunCardCommandAsHuman/CardCommandOptionsForProject, and the real card
// timeline read model — all sharing one connection.
func newCardCommandWebTestHandler(t *testing.T, meta *orchestrator.ProjectMeta) (*WebHandler, db.DBTX, string) {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	projectID := "proj-1"
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: projectID, WorkDir: "/tmp/" + projectID}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	repo := orchestrator.NewTaskRepository(d.Conn)
	jobs := newFakeTriggerJobStore()
	exec := &fakeTriggerExecDispatcher{jobs: jobs}
	workflow := &TaskWorkflowService{
		Triggers:     repo,
		Projects:     orchestrator.NewProjectRepository(d.Conn),
		Meta:         fakeTriggerMetaStore{byProject: map[string]*orchestrator.ProjectMeta{projectID: meta}},
		Jobs:         jobs,
		Exec:         exec,
		CardRequests: repo,
		Tasks:        repo,
		TaskTriage:   repo,
		Tx:           realTransactor{conn: d.Conn},
	}
	svc := cardCommandWebService{
		dbBackedWebService: dbBackedWebService{stubWebService: &stubWebService{}, repo: repo},
		workflow:           workflow,
	}
	h := &WebHandler{
		Service:      svc,
		TaskTriage:   repo,
		CardTimeline: testCardTimelineStore{db: d.Conn},
	}
	return h, d.Conn, projectID
}

func postFormHTML(t *testing.T, h *WebHandler, path string, form url.Values) (int, string, string) {
	t.Helper()
	r := chi.NewRouter()
	r.Mount("/", h.Routes())
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code, w.Body.String(), w.Header().Get("Location")
}

func cardCommandMeta(order []string, commands map[string]orchestrator.CardCommand) *orchestrator.ProjectMeta {
	return &orchestrator.ProjectMeta{CardCommands: commands, CardCommandsOrder: order}
}

// --- declared-order buttons, labels, visibility gates ---

func TestCardDetail_CommandSection_RendersButtonsInDeclaredOrder(t *testing.T) {
	h, repo, projectID := newCardCommandWebTestHandler(t, cardCommandMeta(
		[]string{"zzz", "aaa"},
		map[string]orchestrator.CardCommand{
			"zzz": {Label: "Zeta Command", Run: "echo zzz"},
			"aaa": {Label: "Alpha Command", Run: "echo aaa"},
		},
	))
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	code, body := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", code, body)
	}
	if !strings.Contains(body, `id="card-command-section"`) {
		t.Fatalf("missing card-command-section; got:\n%s", body)
	}
	zetaIdx := strings.Index(body, ">Zeta Command<")
	alphaIdx := strings.Index(body, ">Alpha Command<")
	if zetaIdx == -1 || alphaIdx == -1 {
		t.Fatalf("missing one or both command buttons; got:\n%s", body)
	}
	if zetaIdx > alphaIdx {
		t.Errorf("Zeta Command (declared first) rendered AFTER Alpha Command; want declaration order, got:\n%s", body)
	}
	if !strings.Contains(body, `value="zzz"`) || !strings.Contains(body, `value="aaa"`) {
		t.Errorf("buttons must post the raw command_key as their value; got:\n%s", body)
	}
}

func TestCardDetail_CommandSection_HiddenWhenNoCommandsDeclared(t *testing.T) {
	h, repo, projectID := newCardCommandWebTestHandler(t, cardCommandMeta(nil, nil))
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	code, body := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", code, body)
	}
	if strings.Contains(body, "card-command-section") {
		t.Errorf("card-command-section should not render for a project with no card_commands declared; got:\n%s", body)
	}
}

func TestCardDetail_CommandSection_HiddenOnTerminalCard(t *testing.T) {
	meta := cardCommandMeta([]string{"review"}, map[string]orchestrator.CardCommand{
		"review": {Label: "Run", Run: "echo hi"},
	})
	for _, status := range []orchestrator.TaskStatus{orchestrator.TaskStatusDone, orchestrator.TaskStatusDropped} {
		t.Run(string(status), func(t *testing.T) {
			h, repo, projectID := newCardCommandWebTestHandler(t, meta)
			newCardTimelineTestCard(t, repo, projectID, "card-1")
			task, err := orchestrator.GetTask(repo, "card-1")
			if err != nil {
				t.Fatalf("get task: %v", err)
			}
			task.Status = status
			if err := orchestrator.UpdateTask(repo, task); err != nil {
				t.Fatalf("update task: %v", err)
			}

			code, body := getHTML(t, h, "/tasks/card-1")
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body:\n%s", code, body)
			}
			if strings.Contains(body, "card-command-section") {
				t.Errorf("card-command-section should not render for a %s (terminal) card; got:\n%s", status, body)
			}
		})
	}
}

// --- the daemon renders the label verbatim, never a gloss ---

func TestCardDetail_CommandSection_LabelEscaped(t *testing.T) {
	h, repo, projectID := newCardCommandWebTestHandler(t, cardCommandMeta(
		[]string{"review"},
		map[string]orchestrator.CardCommand{
			"review": {Label: `<script>alert(1)</script>`, Run: "echo hi"},
		},
	))
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	code, body := getHTML(t, h, "/tasks/card-1")
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", code, body)
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("command label must be HTML-escaped, got raw script tag in:\n%s", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("expected the label to appear HTML-escaped; got:\n%s", body)
	}
}

// --- POST: success redirects to the card's own page ---

func TestPostCardCommand_Success_RedirectsToCardOwnPage(t *testing.T) {
	h, repo, projectID := newCardCommandWebTestHandler(t, cardCommandMeta(
		[]string{"review"},
		map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}},
	))
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	code, body, location := postFormHTML(t, h, "/tasks/card-1/commands", url.Values{
		"key":         {"review"},
		"instruction": {"look into this"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body:\n%s", code, body)
	}
	if location != "/tasks/card-1" {
		t.Errorf("Location = %q, want the card's own page %q (never a guessed continuation URL)", location, "/tasks/card-1")
	}
}

func TestPostCardCommand_EmptyInstruction_Succeeds(t *testing.T) {
	h, repo, projectID := newCardCommandWebTestHandler(t, cardCommandMeta(
		[]string{"review"},
		map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}},
	))
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	code, body, location := postFormHTML(t, h, "/tasks/card-1/commands", url.Values{"key": {"review"}})
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (an empty instruction must be allowed); body:\n%s", code, body)
	}
	if location != "/tasks/card-1" {
		t.Errorf("Location = %q, want %q", location, "/tasks/card-1")
	}
}

func TestPostCardCommand_MissingKey_RedirectsWithError(t *testing.T) {
	h, repo, projectID := newCardCommandWebTestHandler(t, cardCommandMeta(
		[]string{"review"},
		map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}},
	))
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	code, _, location := postFormHTML(t, h, "/tasks/card-1/commands", url.Values{"instruction": {"x"}})
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", code)
	}
	if !strings.HasPrefix(location, "/tasks/card-1?error=") {
		t.Errorf("Location = %q, want an error redirect back to the card page", location)
	}
}

// --- POST: occupied preserves the instruction and shows a link only when
// a followable target actually exists ---

func TestPostCardCommand_Occupied_PreservesInstructionAndNoLinkWithoutTarget(t *testing.T) {
	h, repo, projectID := newCardCommandWebTestHandler(t, cardCommandMeta(
		[]string{"review"},
		map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}},
	))
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	// First call claims the slot (still "launching" — no continuation yet).
	if code, body, _ := postFormHTML(t, h, "/tasks/card-1/commands", url.Values{
		"key": {"review"}, "instruction": {"first"},
	}); code != http.StatusSeeOther {
		t.Fatalf("first call: status = %d, want 303; body:\n%s", code, body)
	}

	// Second call finds the slot occupied.
	code, body, location := postFormHTML(t, h, "/tasks/card-1/commands", url.Values{
		"key": {"review"}, "instruction": {"second submission from a different browser tab"},
	})
	if code != http.StatusOK {
		t.Fatalf("occupied call: status = %d, want 200 (render in place, not a redirect); body:\n%s", code, body)
	}
	if location != "" {
		t.Errorf("occupied call must not redirect (that would lose a large instruction over a URL round trip), got Location=%q", location)
	}
	if !strings.Contains(body, "second submission from a different browser tab") {
		t.Errorf("occupied response must echo the caller's OWN just-submitted instruction back into the textarea; got:\n%s", body)
	}
	if strings.Contains(body, "view current run") {
		t.Errorf("no live target exists yet (still launching) — must not render a dead-end link; got:\n%s", body)
	}
	if !strings.Contains(body, "This card's execution slot is busy right now.") {
		t.Errorf("occupied response should say so in neutral (non-error) English; got:\n%s", body)
	}
	if strings.Contains(body, `class="action-error"`) {
		t.Errorf("Occupied is not an error — must not render inside the error-styled block; got:\n%s", body)
	}
}

func TestPostCardCommand_Occupied_ShowsLinkWhenTargetExists(t *testing.T) {
	h, conn, projectID := newCardCommandWebTestHandler(t, cardCommandMeta(
		[]string{"review"},
		map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}},
	))
	newCardTimelineTestCard(t, conn, projectID, "card-1")

	code, body, _ := postFormHTML(t, h, "/tasks/card-1/commands", url.Values{
		"key": {"review"}, "instruction": {"first"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("first call: status = %d, want 303; body:\n%s", code, body)
	}
	repo := orchestrator.NewTaskRepository(conn)
	rows, err := repo.ListCardRequestsByCard("card-1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListCardRequestsByCard: rows=%+v err=%v", rows, err)
	}
	requestID := rows[0].ID

	continuation := &orchestrator.Task{
		ID: "continuation-1", ProjectID: projectID, Type: orchestrator.TaskTypeExecution,
		Exec: &orchestrator.ExecAttrs{Behavior: "executor"},
	}
	if err := repo.CreateTask(continuation); err != nil {
		t.Fatalf("create continuation task: %v", err)
	}
	if err := orchestrator.AttachCardRequest(conn, requestID, orchestrator.CardRequestTargetKindTask, continuation.ID); err != nil {
		t.Fatalf("AttachCardRequest: %v", err)
	}

	// "second-call-marker-xyz" (not just "second"): "btn-secondary" is a CSS
	// class already on the page, so a bare "second" substring-matches inside
	// it regardless of whether the instruction was actually echoed — the
	// exact strings.Contains partial-match trap this PR's own instructions
	// warn against.
	code, body, _ = postFormHTML(t, h, "/tasks/card-1/commands", url.Values{
		"key": {"review"}, "instruction": {"second-call-marker-xyz"},
	})
	if code != http.StatusOK {
		t.Fatalf("occupied call: status = %d, want 200; body:\n%s", code, body)
	}
	wantLink := `href="/tasks/` + continuation.ID + `"`
	if !strings.Contains(body, wantLink) {
		t.Errorf("occupied response missing link to the real continuation (%s); got:\n%s", wantLink, body)
	}
	if !strings.Contains(body, "second-call-marker-xyz") {
		t.Errorf("occupied response must still echo this call's own instruction; got:\n%s", body)
	}
}

func TestPostCardCommand_Occupied_InstructionEscaped(t *testing.T) {
	h, repo, projectID := newCardCommandWebTestHandler(t, cardCommandMeta(
		[]string{"review"},
		map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}},
	))
	newCardTimelineTestCard(t, repo, projectID, "card-1")

	if code, body, _ := postFormHTML(t, h, "/tasks/card-1/commands", url.Values{
		"key": {"review"}, "instruction": {"first"},
	}); code != http.StatusSeeOther {
		t.Fatalf("first call: status = %d, want 303; body:\n%s", code, body)
	}

	code, body, _ := postFormHTML(t, h, "/tasks/card-1/commands", url.Values{
		"key": {"review"}, "instruction": {`</textarea><script>alert(1)</script>`},
	})
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", code, body)
	}
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Errorf("submitted instruction must be HTML-escaped when echoed back; got raw script tag in:\n%s", body)
	}
}

func TestPostCardCommand_TerminalCard_RedirectsWithErrorInsteadOfRendering(t *testing.T) {
	h, repo, projectID := newCardCommandWebTestHandler(t, cardCommandMeta(
		[]string{"review"},
		map[string]orchestrator.CardCommand{"review": {Label: "Run", Run: "echo hi"}},
	))
	newCardTimelineTestCard(t, repo, projectID, "card-1")
	task, err := orchestrator.GetTask(repo, "card-1")
	if err != nil {
		t.Fatalf("get task: %v", err)
	}
	task.Status = orchestrator.TaskStatusDone
	if err := orchestrator.UpdateTask(repo, task); err != nil {
		t.Fatalf("update task: %v", err)
	}

	code, body, location := postFormHTML(t, h, "/tasks/card-1/commands", url.Values{
		"key": {"review"}, "instruction": {"x"},
	})
	if code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 (error redirect, not a 200 render); body:\n%s", code, body)
	}
	if !strings.HasPrefix(location, "/tasks/card-1?error=") {
		t.Errorf("Location = %q, want an error redirect", location)
	}
}
