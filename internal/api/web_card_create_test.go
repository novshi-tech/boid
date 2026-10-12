package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type cardCreationProbe struct {
	WebService
	create func(context.Context, CreateTaskRequest) (*orchestrator.Task, error)
}

func (p cardCreationProbe) CreateTask(ctx context.Context, req CreateTaskRequest) (*orchestrator.Task, error) {
	return p.create(ctx, req)
}

func TestWebCardCreate_AttachmentsReadyBeforeEventAndCleanedOnFailure(t *testing.T) {
	root := t.TempDir()
	stub := &stubWebService{projects: []*orchestrator.Project{testCardProject("proj-1", "default")}}
	var cardID string
	probe := cardCreationProbe{WebService: stub, create: func(_ context.Context, req CreateTaskRequest) (*orchestrator.Task, error) {
		cardID = req.ID
		data, err := ReadAttachment(root, cardID, "ui.png")
		if err != nil || string(data) != "PNGDATA" {
			t.Fatalf("judge could start before attachment was ready: %q, %v", data, err)
		}
		return nil, fmt.Errorf("creation failed")
	}}
	body, contentType := makeMultipartCreateBody(t, "A Card", "see attachment", "ui.png", "image/png", []byte("PNGDATA"))
	r := httptest.NewRequest(http.MethodPost, "/tasks", body)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	(&WebHandler{Service: probe, AttachmentsRoot: root}).PostTaskCreate(w, r)
	if w.Code != http.StatusBadRequest || cardID == "" {
		t.Fatalf("status %d, card %q", w.Code, cardID)
	}
	names, err := ListAttachments(root, cardID)
	if err != nil || len(names) != 0 {
		t.Fatalf("failed creation left attachments: %v, %v", names, err)
	}
}

func testCardProject(id, workspace string) *orchestrator.Project {
	meta := orchestrator.WorkspaceDefaults()
	meta.ID = id
	return &orchestrator.Project{ID: id, WorkspaceID: workspace, Meta: *meta}
}

func TestWebCardCreate_ValidatesWorkspaceAndDestination(t *testing.T) {
	for _, tc := range []struct {
		workspace, project string
		want               int
	}{
		{"", "", http.StatusSeeOther},
		{"team", "", http.StatusSeeOther},
		{"team", "custom", http.StatusSeeOther},
		{"default", "custom", http.StatusBadRequest},
		{"team", "ordinary", http.StatusBadRequest},
		{"missing", "", http.StatusBadRequest},
	} {
		t.Run(tc.workspace+"/"+tc.project, func(t *testing.T) {
			svc := &stubWebService{createTaskResult: &orchestrator.Task{ID: "created"}, workspaces: []*orchestrator.WorkspaceSummary{{ID: "default"}, {ID: "team"}}, projects: []*orchestrator.Project{
				testCardProject("custom", "team"),
				{ID: "ordinary", WorkspaceID: "team"},
			}}
			form := url.Values{"title": {"A goal"}, "workspace": {tc.workspace}, "project_id": {tc.project}}
			r := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(form.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			w := httptest.NewRecorder()
			(&WebHandler{Service: svc}).PostTaskCreate(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if tc.want == http.StatusBadRequest {
				if len(svc.createTaskCalls) != 0 {
					t.Fatal("invalid selection created a task")
				}
			} else if svc.createTaskCalls[0].InitialStatus != "parked" {
				t.Fatal("not a Card")
			}
		})
	}
}

func TestWebCardCreate_DefaultPersistsAndQueuesJudge(t *testing.T) {
	taskSvc, tasks, conn := newSeamService(t)
	ws := orchestrator.NewWorkspaceRepository(conn)
	if err := ws.EnsureDefault(); err != nil {
		t.Fatal(err)
	}
	meta := orchestrator.NewProjectStore()
	projects := orchestrator.NewProjectRepository(conn)
	meta.SetWorkspaceStore(orchestrator.NewWorkspaceStoreWithRepo(t.TempDir(), ws))
	tasks.SetCardEventResolver(meta)
	taskSvc.Tx = seamTransactor{conn: conn, resolver: meta}
	taskSvc.Projects = projects
	taskSvc.Meta = meta
	service := &WebAppService{
		Tasks: tasks, Projects: projects, Meta: meta, TaskSvc: taskSvc,
	}
	h := &WebHandler{Service: service}
	form := url.Values{"title": {"Clarify the next step"}, "description": {"Only a human description"}}
	r := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.PostTaskCreate(w, r)
	if w.Code != http.StatusSeeOther {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	id := strings.TrimPrefix(w.Header().Get("Location"), "/tasks/")
	card, err := tasks.GetTask(id)
	if err != nil {
		t.Fatal(err)
	}
	if card.Type != orchestrator.TaskTypeCard || card.Exec != nil || card.ProjectID != "" || card.WorkspaceID != "default" || card.Description != form.Get("description") {
		t.Fatalf("card = %+v", card)
	}
	requests, err := tasks.ListCardRequestsByCard(id)
	if err != nil || len(requests) != 1 {
		t.Fatalf("requests = %+v, %v", requests, err)
	}
	if requests[0].CommandKey != "judge" || requests[0].Status != orchestrator.CardRequestStatusQueued {
		t.Fatalf("request = %+v", requests[0])
	}
	// The judgment task itself needs neither a repository nor a branch.
	judge, err := taskSvc.CreateTask(context.Background(), CreateTaskRequest{WorkspaceID: card.WorkspaceID, Title: "[judge]", Behavior: "judge"})
	if err != nil {
		t.Fatal(err)
	}
	if judge.Exec == nil || !orchestrator.IsReadonly(judge) || judge.Exec.BaseBranch != "" {
		t.Fatalf("judge = %+v", judge)
	}
}

func TestWebTaskNew_ExcludesFetchProjects(t *testing.T) {
	push := testCardProject("push-project", "default")
	push.Perm = "push"
	fetch := testCardProject("fetch-project", "default")
	fetch.Perm = "fetch"
	svc := &stubWebService{projects: []*orchestrator.Project{push, fetch}}
	w := httptest.NewRecorder()
	(&WebHandler{Service: svc}).TaskNew(w, httptest.NewRequest(http.MethodGet, "/tasks/new", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `value="push-project"`) || strings.Contains(body, `value="fetch-project"`) {
		t.Fatalf("project choices do not respect permissions: %s", body)
	}
}

func TestWebWorkspaceOnlyCard(t *testing.T) {
	svc := &stubWebService{workspaces: []*orchestrator.WorkspaceSummary{{ID: "team"}}, createTaskResult: &orchestrator.Task{ID: "created"}}
	handler := &WebHandler{Service: svc}
	w := httptest.NewRecorder()
	handler.TaskNew(w, httptest.NewRequest(http.MethodGet, "/tasks/new?workspace=team", nil))
	if !strings.Contains(w.Body.String(), `value="team"`) || !strings.Contains(w.Body.String(), "Workspace only") {
		t.Fatal(w.Body.String())
	}
	form := url.Values{"title": {"work"}, "workspace": {"team"}, "project_id": {"__workspace__"}}
	request := httptest.NewRequest(http.MethodPost, "/tasks", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w = httptest.NewRecorder()
	handler.PostTaskCreate(w, request)
	if w.Code != http.StatusSeeOther || len(svc.createTaskCalls) != 1 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	req := svc.createTaskCalls[0]
	if req.ProjectID != "" || req.WorkspaceID != "team" {
		t.Fatalf("request: %+v", req)
	}
}
