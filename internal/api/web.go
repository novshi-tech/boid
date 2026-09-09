package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/timeline"
	"github.com/novshi-tech/boid/web/templates"
)

// detailTimelineGroups builds the status-grouped timeline for the Web UI
// task detail page. The shared timeline package groups actions and jobs
// into status-visit sections; we convert api.Job → timeline.JobInfo here
// so the timeline package stays api-free (which keeps it importable from
// web/templates without cycling through internal/api).
func detailTimelineGroups(detail *TaskDetailView) []timeline.StatusGroup {
	if detail == nil || detail.Task == nil {
		return nil
	}
	infos := make([]*timeline.JobInfo, 0, len(detail.Jobs))
	for _, j := range detail.Jobs {
		if j == nil {
			continue
		}
		infos = append(infos, &timeline.JobInfo{
			ID:          j.ID,
			Role:        j.Role,
			HandlerID:   j.HandlerID,
			DisplayName: j.DisplayName,
			Status:      string(j.Status),
			ExitCode:    j.ExitCode,
			CreatedAt:   j.CreatedAt,
			UpdatedAt:   j.UpdatedAt,
		})
	}
	return timeline.Build(detail.Task, detail.Actions, infos)
}

type WebHandler struct {
	Service           WebService
	Hub               *TaskEventHub
	SessionDispatcher SessionDispatcher
	Registry          *auth.ConnectionRegistry

	// AttachmentsRoot is the data-home directory under which per-task
	// attachments (`tasks/<id>/attachments`) are persisted. When empty (e.g.
	// :memory: DB during tests) the multipart code path falls back to
	// rejecting attachments while still accepting plain form-urlencoded
	// submissions.
	AttachmentsRoot string

	// ConfigService backs GET /settings — nil in any test/wiring that never
	// registers the /settings route.
	ConfigService SettingsConfigService

	// TaskTriage backs the list row's suggestion/summary enrichment.
	// Nil-safe: when unset, every row renders with no suggestion edge/
	// summary badge instead of failing the whole list.
	TaskTriage CardStore

	// CardActivity backs the list row's activity state: the sole work
	// child's Draft/Ready to run/Queued/Running/Needs input badge and the
	// active card command's label+status badge. Nil-safe: when unset, every
	// card row renders with no activity badge instead of failing the whole
	// list.
	CardActivity CardActivityStore

	// CardTimeline backs the card detail page's pinned items and timeline
	// history (internal/timeline.BuildCardTimeline/CardPinnedItems). Nil-safe:
	// when unset, a card detail page renders with an empty timeline instead of
	// failing the whole page.
	CardTimeline CardTimelineStore

	// OperationResults keeps human operation outcomes visible across reloads.
	OperationResults OperationResultStore
}

func (h *WebHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.TaskList)
	r.Get("/tasks/new", h.TaskNew)
	r.Post("/tasks", h.PostTaskCreate)
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Get("/tasks/{id}/children/{child_id}", h.TaskChildDetail)
	r.Get("/tasks/{id}/fragment", h.TaskDetailFragment)
	r.Get("/tasks/{id}/card-timeline", h.TaskCardTimelineOlder)
	r.Get("/tasks/{id}/card-timeline/head", h.TaskCardTimelineHead)
	r.Post("/tasks/{id}/commands", h.PostCardCommand)
	r.Get("/tasks/{id}/edit", h.GetTaskEdit)
	r.Post("/tasks/{id}/edit", h.PostEdit)
	r.Post("/tasks/{id}/action", h.PostAction)
	r.Post("/tasks/{id}/suggestion", h.PostAnswerSuggestion)
	r.Post("/tasks/{id}/duplicate", h.PostDuplicate)
	r.Post("/tasks/{id}/rerun", h.PostRerun)
	r.Get("/tasks/{id}/reopen", h.ReopenForm)
	r.Post("/tasks/{id}/reopen", h.PostReopen)
	r.Post("/tasks/{id}/delete", h.PostDelete)
	r.Post("/tasks/{id}/answer", h.PostAnswer)
	r.Get("/tasks/{id}/questions/{question_id}", h.QuestionPage)
	r.Get("/tasks/{id}/hooks", h.HookReplayList)
	r.Post("/tasks/{id}/hooks/{hook_id}/replay", h.PostHookReplay)
	r.Get("/sessions", h.SessionList)
	r.Get("/sessions/new", h.SessionNew)
	r.Get("/jobs/{id}", h.JobDetail)
	r.Get("/jobs/{id}/terminal", h.JobTerminal)
	r.Post("/projects/{id}/sessions/start", h.PostStartSession)
	r.Get("/settings", h.Settings)
	return r
}

// redirectTask redirects the client to the task detail page.
func redirectTask(w http.ResponseWriter, r *http.Request, id string) {
	http.Redirect(w, r, "/tasks/"+id, http.StatusSeeOther)
}

// redirectTaskErr redirects the client to the task detail page with err
// surfaced via the ?error= query parameter.
func redirectTaskErr(w http.ResponseWriter, r *http.Request, id string, err error) {
	http.Redirect(w, r, "/tasks/"+id+"?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
}

// redirectOrHXRedirect redirects to target. htmx form posts get the
// HX-Redirect response header plus a 200 status instead of a 3xx response,
// since htmx does not follow standard redirects for non-GET requests.
func redirectOrHXRedirect(w http.ResponseWriter, r *http.Request, target string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// renderTaskNewErr re-renders the "new task" form with msg surfaced as a
// validation error, preserving the previously submitted form values.
func (h *WebHandler) renderTaskNewErr(w http.ResponseWriter, r *http.Request, msg string, form url.Values) {
	projects, _ := h.Service.ListProjects()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	templates.TaskNew(projects, msg, form).Render(r.Context(), w)
}

func (h *WebHandler) TaskNew(w http.ResponseWriter, r *http.Request) {
	projects, _ := h.Service.ListProjects()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskNew(projects, "", nil).Render(r.Context(), w)
}

func (h *WebHandler) PostTaskCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseTaskForm(r); err != nil {
		h.renderTaskNewErr(w, r, "リクエストの解析に失敗しました", nil)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	if title == "" {
		h.renderTaskNewErr(w, r, "タイトルは必須です", r.PostForm)
		return
	}

	req := CreateTaskRequest{
		ProjectID:   r.FormValue("project_id"),
		Title:       title,
		Behavior:    r.FormValue("behavior"),
		Description: r.FormValue("description"),
		RemoteID:    r.FormValue("remote_id"),
		ParentID:    r.FormValue("parent_id"),
		AutoStart:   r.FormValue("auto_start") == "on",
	}

	if raw := strings.TrimSpace(r.FormValue("traits")); raw != "" {
		req.Traits = strings.Fields(raw)
	}

	agent := strings.TrimSpace(r.FormValue("agent"))
	model := strings.TrimSpace(r.FormValue("model"))
	if agent != "" || model != "" {
		instsJSON, err := json.Marshal(orchestrator.Instructions{{Agent: agent, Model: model}})
		if err != nil {
			h.renderTaskNewErr(w, r, err.Error(), r.PostForm)
			return
		}
		req.Instructions = instsJSON
	}

	uploads := taskFormAttachments(r)
	if len(uploads) > 0 {
		if h.AttachmentsRoot == "" {
			h.renderTaskNewErr(w, r, "添付ファイルを保存する場所が設定されていません", r.PostForm)
			return
		}
		if err := ValidateAttachmentHeaders(uploads); err != nil {
			h.renderTaskNewErr(w, r, err.Error(), r.PostForm)
			return
		}
	}

	task, err := h.Service.CreateTask(r.Context(), req)
	if err != nil {
		h.renderTaskNewErr(w, r, err.Error(), r.PostForm)
		return
	}

	if len(uploads) > 0 {
		if _, err := SaveMultipartAttachments(h.AttachmentsRoot, task.ID, uploads); err != nil {
			// Task is already created — surface the error via ?error= so the
			// user sees the task page with the failure context and can decide
			// whether to retry, delete, or proceed.
			redirectTaskErr(w, r, task.ID, fmt.Errorf("attachment save failed: %w", err))
			return
		}
	} else if h.AttachmentsRoot != "" {
		// Always pre-create the attachments dir so subsequent task-ask
		// answers can drop files into a live-bound location. Failure here is
		// non-fatal — the bind has an optional guard and the worst case is
		// the user re-attaches after we recover.
		_, _ = EnsureAttachmentsDir(h.AttachmentsRoot, task.ID)
	}

	redirectTask(w, r, task.ID)
}

// parseTaskForm dispatches on Content-Type so the same handler accepts both
// the legacy application/x-www-form-urlencoded submissions (still used by
// older clients and HTML form fallbacks) and the new multipart/form-data
// uploads coming from the clipboard-paste flow.
//
// net/http's ParseMultipartForm returns ErrNotMultipart for non-multipart
// bodies, so blindly calling it would break every existing form post — keep
// the explicit branch.
func parseTaskForm(r *http.Request) error {
	ct := r.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "multipart/") {
		// 32 MB ceiling — slightly above the 30 MB per-task total cap so the
		// multipart parser doesn't reject a borderline-legal request before
		// our per-file / per-task limits kick in.
		return r.ParseMultipartForm(32 << 20)
	}
	return r.ParseForm()
}

// taskFormAttachments extracts uploaded files from the "attachments" multipart
// field. Safe to call when the request has no multipart body — it returns
// nil in that case.
func taskFormAttachments(r *http.Request) []*multipart.FileHeader {
	if r.MultipartForm == nil || r.MultipartForm.File == nil {
		return nil
	}
	return r.MultipartForm.File["attachments"]
}

// triageByTaskID batch-fetches task_triage rows for the list row's
// suggestion/summary enrichment in one query rather than a per-task loop.
// h.TaskTriage == nil degrades to no enrichment (empty map), not an error.
// A non-nil err from ListTaskTriageByTaskIDs means at least one row/chunk
// failed but out may still be partially populated; using it rather than
// discarding to an empty map preserves "don't let one bad row sink the
// list" for the rows that did succeed. The error is logged, not swallowed,
// so missing badges are diagnosable.
func (h *WebHandler) triageByTaskID(tasks []*orchestrator.Task) map[string]*orchestrator.CardAttrs {
	if h.TaskTriage == nil {
		return map[string]*orchestrator.CardAttrs{}
	}
	ids := make([]string, len(tasks))
	for i, t := range tasks {
		ids[i] = t.ID
	}
	out, err := h.TaskTriage.ListTaskTriageByTaskIDs(ids)
	if err != nil {
		slog.Warn("triageByTaskID: ListTaskTriageByTaskIDs returned a partial or empty result",
			"error", err, "task_count", len(ids), "rows_returned", len(out))
	}
	if out == nil {
		return map[string]*orchestrator.CardAttrs{}
	}
	return out
}

// cardActivityStates computes the list row's activity state for every card
// in tasks, from triageByTaskID's already-fetched detail JSON plus two more
// batched reads (h.CardActivity) — never one query per row. h.CardActivity
// == nil degrades to no activity badges, not an error, same convention as
// triageByTaskID/h.TaskTriage.
func (h *WebHandler) cardActivityStates(tasks []*orchestrator.Task, triage map[string]*orchestrator.CardAttrs) map[string]templates.CardActivityState {
	if h.CardActivity == nil {
		return nil
	}
	var cardIDs []string
	activeChildren := map[string]*orchestrator.TaskTriageChild{}
	for _, t := range tasks {
		if t.Type != orchestrator.TaskTypeCard {
			continue
		}
		cardIDs = append(cardIDs, t.ID)
		tt, ok := triage[t.ID]
		if !ok || tt == nil {
			continue
		}
		if child := templates.ActiveChildFromDetail(tt.Detail); child != nil {
			activeChildren[t.ID] = child
		}
	}
	if len(cardIDs) == 0 {
		return nil
	}

	activeRequests, err := h.CardActivity.ActiveCardRequestsByCardIDs(cardIDs)
	activityUnavailable := err != nil
	if err != nil {
		slog.Warn("cardActivityStates: ActiveCardRequestsByCardIDs returned a partial or empty result",
			"error", err, "card_count", len(cardIDs))
	}

	var taskIDsToCheck []string
	for _, c := range activeChildren {
		if c.Status == orchestrator.TaskTriageChildStatusDispatched && c.TaskRef != "" {
			taskIDsToCheck = append(taskIDsToCheck, c.TaskRef)
		}
	}
	for _, r := range activeRequests {
		if r.TargetKind == orchestrator.CardRequestTargetKindTask && r.TargetID != "" {
			taskIDsToCheck = append(taskIDsToCheck, r.TargetID)
		}
	}
	taskStatuses, err := h.CardActivity.TaskStatusesByIDs(taskIDsToCheck)
	activityUnavailable = activityUnavailable || err != nil
	if err != nil {
		slog.Warn("cardActivityStates: TaskStatusesByIDs returned a partial or empty result",
			"error", err, "task_id_count", len(taskIDsToCheck))
	}
	states := templates.BuildCardActivityStates(cardIDs, activeChildren, taskStatuses, activeRequests)
	for _, task := range tasks {
		if task.Type != orchestrator.TaskTypeCard || orchestrator.IsTerminalStatus(task.Status) {
			continue
		}
		state := states[task.ID]
		if activityUnavailable {
			state.DecisionLabel = "Activity unavailable"
			states[task.ID] = state
			continue
		}
		if activeChildren[task.ID] == nil && state.CommandLabel == "" && task.OpenChildCount == 0 && task.DoneChildCount+task.AbortedChildCount > 0 {
			state.DecisionLabel = "Work finished · Needs decision"
			states[task.ID] = state
		}
	}
	return states
}

// taskListPageSize is the list's fixed page size — no user-configurable
// page-size control.
const taskListPageSize = 50

// parseTaskListPage reads the "page" query param as a 1-indexed page
// number, clamped to >= 1 for any missing/malformed/non-positive value —
// a bad or absent page param must degrade to "show page 1", never a 500 or
// a negative OFFSET.
func parseTaskListPage(raw string) int {
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// taskFilterCookieName holds the browser-side memory of the task list's
// filter query (status/project/behavior/workspace/q/active), so a plain "/"
// visit (a bookmark, the nav link, a browser restore) reapplies whatever the
// user last narrowed the list to instead of resetting to "show everything".
// This is client-side persistence, not server state — the daemon itself
// never reads or reasons about this cookie's value beyond replaying it back
// into the same query-param handling every other visit already goes
// through.
const taskFilterCookieName = "boid_task_filters"

// taskFilterCookieMaxAge is deliberately long (a browser profile is
// typically long-lived) — this is a convenience default, not a security or
// privacy-sensitive TTL.
const taskFilterCookieMaxAge = 180 * 24 * time.Hour

// taskFilterCookieKeys lists the query params that make up the "filter"
// (as opposed to "page", which is pagination state, not a filter, and
// "cleared", which is a one-shot signal rather than a persisted value).
func taskFilterCookieKeys() []string {
	return []string{"status", "project", "behavior", "workspace", "q", "active"}
}

// encodeTaskFilterCookie extracts just the filter keys from q and re-encodes
// them as a query string suitable for both the cookie value and a redirect
// target.
func encodeTaskFilterCookie(q url.Values) string {
	v := url.Values{}
	for _, k := range taskFilterCookieKeys() {
		if val := q.Get(k); val != "" {
			v.Set(k, val)
		}
	}
	return v.Encode()
}

func deleteTaskFilterCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: taskFilterCookieName, Value: "", Path: "/", MaxAge: -1})
}

func (h *WebHandler) TaskList(w http.ResponseWriter, r *http.Request) {
	// The cookie dance below only applies to a real top-level browser
	// navigation — htmx's own requests (the 5s poll, the filter form's
	// change/input handlers, pagination) always carry an explicit query
	// (possibly empty-filter-but-present, e.g. the poll re-requesting the
	// exact currentURL it was given) and must render inline; redirecting one
	// would break the swap/poll loop.
	if r.Header.Get("HX-Request") != "true" {
		if _, cleared := r.URL.Query()["cleared"]; cleared {
			deleteTaskFilterCookie(w)
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if r.URL.RawQuery == "" {
			if c, err := r.Cookie(taskFilterCookieName); err == nil && c.Value != "" {
				http.Redirect(w, r, "/?"+c.Value, http.StatusFound)
				return
			}
		}
	}

	q := r.URL.Query()
	if r.URL.RawQuery != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     taskFilterCookieName,
			Value:    encodeTaskFilterCookie(q),
			Path:     "/",
			MaxAge:   int(taskFilterCookieMaxAge.Seconds()),
			SameSite: http.SameSiteLaxMode,
		})
	}
	page := parseTaskListPage(q.Get("page"))
	// The list is one flat, top-level-only view: ParentID="" restricts to
	// root tasks (a card's/root exec task's own children are read from the
	// detail page instead). Default status is "" (every status,
	// newest-updated first); ActiveOnly is the opt-in "アクティブのみ" narrowing.
	rootParentID := ""
	filter := orchestrator.TaskFilter{
		Status:      q.Get("status"),
		ProjectID:   q.Get("project"),
		Behavior:    q.Get("behavior"),
		WorkspaceID: q.Get("workspace"),
		Title:       q.Get("q"),
		ParentID:    &rootParentID,
		ActiveOnly:  q.Get("active") == "1",
		// Fetch one row past the page size to answer "is there a next page"
		// without a second COUNT query — trimmed back to taskListPageSize
		// below before it ever reaches a template.
		Limit:  taskListPageSize + 1,
		Offset: (page - 1) * taskListPageSize,
	}

	projects, _ := h.Service.ListProjects()
	projects = filterProjectsByWorkspace(projects, filter.WorkspaceID)
	if !projectInList(projects, filter.ProjectID) {
		filter.ProjectID = ""
	}

	tasks, err := h.Service.ListTasks(filter)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	hasMore := len(tasks) > taskListPageSize
	if hasMore {
		tasks = tasks[:taskListPageSize]
	}

	projectNames := projectNameMap(projects)
	triage := h.triageByTaskID(tasks)
	items := templates.BuildListRows(tasks, projectNames, triage, h.cardActivityStates(tasks, triage))

	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	if r.Header.Get("HX-Target") == "main-content" {
		workspaces, _ := h.Service.ListWorkspaces()
		templates.TaskListContent(items, filter, page, hasMore, projects, workspaces, r.URL.RequestURI()).Render(r.Context(), w)
		return
	}

	if r.Header.Get("HX-Request") == "true" {
		templates.TaskListFragment(items, filter, page, hasMore, r.URL.RequestURI()).Render(r.Context(), w)
		return
	}

	workspaces, _ := h.Service.ListWorkspaces()
	templates.TaskList(items, filter, page, hasMore, projects, workspaces, r.URL.RequestURI()).Render(r.Context(), w)
}

func (h *WebHandler) SessionList(w http.ResponseWriter, r *http.Request) {
	projectFilter := r.URL.Query().Get("project")
	jobs, err := h.Service.ListSessions()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if projectFilter != "" {
		filtered := jobs[:0]
		for _, j := range jobs {
			if j.ProjectID == projectFilter {
				filtered = append(filtered, j)
			}
		}
		jobs = filtered
	}
	sessions := make([]templates.SessionView, 0, len(jobs))
	for _, j := range jobs {
		sessions = append(sessions, templates.SessionView{
			ID:          j.ID,
			ProjectID:   j.ProjectID,
			ProjectName: j.ProjectName,
			HandlerID:   j.HandlerID,
			DisplayName: j.DisplayName,
			CreatedAt:   j.CreatedAt,
		})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.SessionList(sessions, projectFilter).Render(r.Context(), w)
}

func (h *WebHandler) SessionNew(w http.ResponseWriter, r *http.Request) {
	projects, _ := h.Service.ListProjects()
	selectedProjectID := r.URL.Query().Get("project")
	errorMsg := r.URL.Query().Get("error")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.SessionNew(projects, selectedProjectID, errorMsg).Render(r.Context(), w)
}

// filterProjectsByWorkspace filters projects to only those in the given workspace.
// If workspaceID is empty, all projects are returned.
func filterProjectsByWorkspace(projects []*orchestrator.Project, workspaceID string) []*orchestrator.Project {
	if workspaceID == "" {
		return projects
	}
	filtered := make([]*orchestrator.Project, 0, len(projects))
	for _, p := range projects {
		if p.WorkspaceID == workspaceID {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

// projectInList returns true if projectID is empty or found in the project list.
func projectInList(projects []*orchestrator.Project, projectID string) bool {
	if projectID == "" {
		return true
	}
	for _, p := range projects {
		if p.ID == projectID {
			return true
		}
	}
	return false
}

// projectNameMap builds an id→display-name lookup from a project list.
func projectNameMap(projects []*orchestrator.Project) map[string]string {
	m := make(map[string]string, len(projects))
	for _, p := range projects {
		if p.Meta.Name != "" {
			m[p.ID] = p.Meta.Name
		}
	}
	return m
}

func (h *WebHandler) TaskDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	tab := r.URL.Query().Get("tab")
	if tab == "" {
		tab = "timeline"
	}
	errorMsg := r.URL.Query().Get("error")
	timelineGroups := detailTimelineGroups(detail)

	// A card detail page has no tabs, so there is nothing for an HX-Request
	// tab-swap to target — a card always falls through to the full-page
	// render below. Only an execution task's tab click takes this shortcut.
	if r.Header.Get("HX-Request") == "true" && detail.Task.Type != orchestrator.TaskTypeCard {
		childTree, childErr := h.execChildTree(detail.Task)
		if childErr != nil {
			http.Error(w, "Unable to load subtasks", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		templates.TaskDetailExecTabsSection(detail.Task, timelineGroups, detail.AvailableActions, tab, childTree).Render(r.Context(), w)
		return
	}

	h.renderTaskDetailPage(w, r, detail, tab, errorMsg, nil, nil)
}

// TaskChildDetail is the stable entry point for a card child. It resolves
// to the execution task once one exists; before dispatch (or after task GC)
// it renders the specification that remains on the parent card.
func (h *WebHandler) TaskChildDetail(w http.ResponseWriter, r *http.Request) {
	parentID := chi.URLParam(r, "id")
	childID := chi.URLParam(r, "child_id")
	parent, err := h.Service.GetTaskDetail(parentID)
	if err != nil {
		if errors.Is(err, orchestrator.ErrTaskNotFound) {
			http.Error(w, "Parent card not found", http.StatusNotFound)
		} else {
			http.Error(w, "Parent card is temporarily unavailable", http.StatusInternalServerError)
		}
		return
	}
	if parent == nil || parent.Task == nil || parent.Task.Type != orchestrator.TaskTypeCard {
		http.Error(w, "Parent card not found", http.StatusNotFound)
		return
	}
	if h.TaskTriage == nil {
		http.Error(w, "Child not found", http.StatusNotFound)
		return
	}
	triage, triageErr := h.TaskTriage.GetTaskTriage(parentID)
	if triageErr != nil {
		if errors.Is(triageErr, sql.ErrNoRows) || errors.Is(triageErr, orchestrator.ErrTaskNotFound) {
			http.Error(w, "Child not found", http.StatusNotFound)
		} else {
			http.Error(w, "Child details are temporarily unavailable", http.StatusInternalServerError)
		}
		return
	}
	if triage == nil {
		http.Error(w, "Child not found", http.StatusNotFound)
		return
	}
	children, err := orchestrator.DetailChildren(triage.Detail)
	if err != nil {
		http.Error(w, "Child details are unavailable", http.StatusInternalServerError)
		return
	}
	for _, child := range children {
		if child.ID != childID {
			continue
		}
		if child.TaskRef != "" {
			detail, detailErr := h.Service.GetTaskDetail(child.TaskRef)
			switch {
			case detailErr == nil && detail != nil && detail.Task != nil && detail.Task.ID == child.TaskRef && detail.Task.ParentID == parentID:
				http.Redirect(w, r, "/tasks/"+child.TaskRef, http.StatusSeeOther)
				return
			case detailErr != nil && !errors.Is(detailErr, orchestrator.ErrTaskNotFound):
				http.Error(w, "Child execution task is temporarily unavailable", http.StatusInternalServerError)
				return
			}
		}
		result, resultStatus, hasResult, resultErr := h.cardChildResult(parentID, childID)
		if resultErr != nil {
			http.Error(w, "Child result is temporarily unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		templates.TaskChildSpecDetail(parent.Task, child, child.TaskRef != "", result, resultStatus, hasResult).Render(r.Context(), w)
		return
	}
	http.Error(w, "Child not found", http.StatusNotFound)
}

func (h *WebHandler) cardChildResult(cardID, childID string) (string, orchestrator.TaskStatus, bool, error) {
	if h.CardTimeline == nil {
		return "", "", false, nil
	}
	cursor := ""
	for pageCount := 0; pageCount < 20; pageCount++ {
		page, err := h.CardTimeline.BuildCardTimeline(cardID, cursor, timeline.MaxCardTimelineLimit)
		if err != nil {
			return "", "", false, err
		}
		if page == nil {
			return "", "", false, fmt.Errorf("card timeline returned no page")
		}
		for _, item := range page.Items {
			if item.Child != nil && item.Child.ChildID == childID && item.Child.HasResult {
				return item.Child.Result, item.Child.ResultStatus, true, nil
			}
		}
		if !page.HasMore || page.NextCursor == "" || page.NextCursor == cursor {
			return "", "", false, nil
		}
		cursor = page.NextCursor
	}
	return "", "", false, fmt.Errorf("child result exceeds timeline scan limit")
}

// renderTaskDetailPage assembles and renders a task/card detail page's full
// body. cmdForm is non-nil for a failed card-command submission so the direct
// response can preserve the user's instruction; see CardCommandFormState.
func (h *WebHandler) renderTaskDetailPage(w http.ResponseWriter, r *http.Request, detail *TaskDetailView, tab, errorMsg string, cmdForm *templates.CardCommandFormState, transient *orchestrator.OperationResult) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	timelineGroups := detailTimelineGroups(detail)
	projectName := h.lookupProjectName(detail.Task.ProjectID)
	var childTree []templates.ChildTreeNode
	var cardSummary string
	var cardTL *templates.CardTimelineView
	var cmdOptions []templates.CardCommandOption
	if detail.Task.Type == orchestrator.TaskTypeCard {
		if triage := h.loadTriage(detail.Task.ID); triage != nil {
			cardSummary = templates.TriageSummary(triage.Detail)
		}
		var tlErr error
		cardTL, tlErr = h.cardTimelineView(detail.Task.ID, "")
		if tlErr != nil {
			slog.Warn("card timeline view failed", "task_id", detail.Task.ID, "error", tlErr)
			errorMsg = strings.TrimSpace(errorMsg + " Unable to load current activity. Refresh status to retry.")
			cardTL = &templates.CardTimelineView{}
		}
		cmdOptions = toTemplateCardCommandOptions(h.Service.CardCommandOptionsForProject(r.Context(), detail.Task.ProjectID))
	} else {
		var childErr error
		childTree, childErr = h.execChildTree(detail.Task)
		if childErr != nil {
			slog.Warn("execution child view failed", "task_id", detail.Task.ID, "error", childErr)
			errorMsg = strings.TrimSpace(errorMsg + " Unable to load subtasks. Refresh to retry.")
		}
	}
	operationResults, operationErr := h.operationResultsForRequest(r, detail.Task.ID)
	if operationErr != nil {
		slog.Warn("operation results unavailable", "task_id", detail.Task.ID, "error", operationErr)
		errorMsg = strings.TrimSpace(errorMsg + " Operation results are temporarily unavailable. Refresh to retry.")
	}
	if transient != nil {
		operationResults = promoteOperationResult(operationResults, transient)
	}
	templates.TaskDetail(detail.Task, timelineGroups, detail.AvailableActions, errorMsg, tab, projectName, cardSummary, cardTL, childTree, cmdOptions, cmdForm, detail.Identities, operationResults).Render(r.Context(), w)
}

func (h *WebHandler) operationResultsForRequest(r *http.Request, taskID string) ([]*orchestrator.OperationResult, error) {
	if h.OperationResults == nil {
		return nil, nil
	}
	results, err := h.OperationResults.ListOperationResults(taskID, 20)
	if err != nil {
		return nil, err
	}
	if requestedID := r.URL.Query().Get("operation"); requestedID != "" {
		requested, err := h.OperationResults.GetOperationResult(taskID, requestedID)
		if errors.Is(err, sql.ErrNoRows) {
			return results, nil
		}
		if err != nil {
			return results, err
		}
		if requested != nil {
			results = promoteOperationResult(results, requested)
		}
	}
	return results, nil
}

func promoteOperationResult(results []*orchestrator.OperationResult, current *orchestrator.OperationResult) []*orchestrator.OperationResult {
	out := []*orchestrator.OperationResult{current}
	for _, result := range results {
		if result.ID != current.ID {
			out = append(out, result)
		}
	}
	return out
}

// toTemplateCardCommandOptions converts the api-layer option list to
// web/templates' own display copy (that package cannot import internal/api).
func toTemplateCardCommandOptions(opts []CardCommandOption) []templates.CardCommandOption {
	if len(opts) == 0 {
		return nil
	}
	out := make([]templates.CardCommandOption, len(opts))
	for i, o := range opts {
		out[i] = templates.CardCommandOption{Key: o.Key, Label: o.Label}
	}
	return out
}

// PostCardCommand handles the card detail page's command form: the shared
// instruction textarea plus one submit button per declared card_commands
// entry (the clicked button's own name="key" value identifies which command
// to run).
//
// On success it redirects to the card's own page — never a guessed
// continuation URL — and lets the pinned section resolve and link the new
// request's continuation once one exists. On Occupied it renders the same
// page directly instead of redirecting, echoing the just-submitted
// instruction back into the form: a redirect would have to round-trip a
// possibly large instruction through a URL, and the caller's typed text
// must survive an occupied response regardless.
func (h *WebHandler) PostCardCommand(w http.ResponseWriter, r *http.Request) {
	receivedAt := time.Now().UTC()
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	key := r.FormValue("key")
	instruction := r.FormValue("instruction")
	op := pendingOperation(id, "card_command:"+key, h.operationLabelForCommand(r.Context(), id, key), receivedAt)
	if err := h.createPendingOperation(op); err != nil {
		h.renderOperationUnavailable(w, r, id, instruction, true)
		return
	}
	if key == "" {
		h.finishCardCommand(w, r, id, instruction, op, operationRejection(id, op.OperationType, op.OperationLabel, &StatusError{Code: http.StatusBadRequest, Message: "command key is required"}), http.StatusBadRequest)
		return
	}

	result, err := h.Service.RunCardCommandAsHuman(r.Context(), id, key, instruction)
	if err != nil {
		h.finishCardCommand(w, r, id, instruction, op, operationRejection(id, op.OperationType, op.OperationLabel, err), statusCodeForOperationError(err))
		return
	}
	if !result.Occupied {
		outcome := &orchestrator.OperationResult{TaskID: id, OperationType: op.OperationType, OperationLabel: op.OperationLabel,
			Result: orchestrator.OperationResultAccepted, ReasonCode: orchestrator.OperationReasonRequestAccepted,
			TargetRequestID: result.RequestID}
		if updateErr := h.finalizeOperation(op, outcome); updateErr != nil {
			op.Notice = operationReceiptUpdateWarning
			h.renderCardCommandState(w, r, id, instruction, http.StatusOK, "", op)
			return
		}
		http.Redirect(w, r, "/tasks/"+id+"?operation="+url.QueryEscape(op.ID), http.StatusSeeOther)
		return
	}
	outcome := &orchestrator.OperationResult{TaskID: id, OperationType: op.OperationType, OperationLabel: op.OperationLabel,
		Result: orchestrator.OperationResultRejected, ReasonCode: orchestrator.OperationReasonSlotOccupied,
		TargetRequestID: result.RequestID}
	if result.TargetKind == orchestrator.CardRequestTargetKindTask {
		outcome.TargetTaskID = result.TargetID
	}
	if result.TargetKind == orchestrator.CardRequestTargetKindSession {
		outcome.TargetSessionID = result.TargetID
	}
	h.finishCardCommand(w, r, id, instruction, op, outcome, http.StatusConflict)
}

func (h *WebHandler) finishCardCommand(w http.ResponseWriter, r *http.Request, id, instruction string, pending, outcome *orchestrator.OperationResult, code int) {
	if err := h.finalizeOperation(pending, outcome); err != nil {
		pending.Notice = operationReceiptUpdateWarning
	}
	h.renderCardCommandState(w, r, id, instruction, code, "", pending)
}

func (h *WebHandler) renderCardCommandState(w http.ResponseWriter, r *http.Request, id, instruction string, code int, message string, transient *orchestrator.OperationResult) {
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if transient != nil {
		w.Header().Set("X-Boid-Operation-ID", transient.ID)
	}
	w.WriteHeader(code)
	h.renderTaskDetailPage(w, r, detail, "timeline", message, &templates.CardCommandFormState{Instruction: instruction}, transient)
}

func (h *WebHandler) operationLabelForCommand(ctx context.Context, taskID, key string) string {
	detail, err := h.Service.GetTaskDetail(taskID)
	if err == nil && detail != nil && detail.Task != nil {
		for _, option := range h.Service.CardCommandOptionsForProject(ctx, detail.Task.ProjectID) {
			if option.Key == key && option.Label != "" {
				return option.Label
			}
		}
	}
	if key == "" {
		return "Card command"
	}
	return key
}

// execChildTree returns a task's direct children. Every execution detail
// page calls it, regardless of the task's own depth; opening a child repeats
// the same one-level navigation without recursively expanding descendants.
func (h *WebHandler) execChildTree(task *orchestrator.Task) ([]templates.ChildTreeNode, error) {
	if task == nil {
		return nil, nil
	}
	parentID := task.ID
	kids, err := h.Service.ListTasks(orchestrator.TaskFilter{ParentID: &parentID})
	if err != nil {
		return nil, fmt.Errorf("list direct children: %w", err)
	}
	if len(kids) == 0 {
		return nil, nil
	}
	nodes := make([]templates.ChildTreeNode, 0, len(kids))
	for _, k := range kids {
		node := templates.ChildTreeNode{Task: k, CreatedAt: k.CreatedAt, HasCreatedAt: !k.CreatedAt.IsZero()}
		if k.Exec != nil && k.Status == orchestrator.TaskStatusAwaiting {
			node.QuestionID = orchestrator.GetAwaitingPayload(k.Exec.Payload).QuestionID
		}
		detail, detailErr := h.Service.GetTaskDetail(k.ID)
		if detailErr != nil {
			return nil, fmt.Errorf("load child %s history: %w", k.ID, detailErr)
		}
		if detail == nil || detail.Task == nil || detail.Task.ID != k.ID {
			return nil, fmt.Errorf("load child %s history: inconsistent task detail", k.ID)
		}
		for _, action := range detail.Actions {
			if action != nil && orchestrator.IsTerminalStatus(action.ToStatus) {
				node.TerminalEvents = append(node.TerminalEvents, templates.ChildTerminalEvent{
					Status: action.ToStatus, Time: action.CreatedAt, HasTime: !action.CreatedAt.IsZero(),
				})
			}
		}
		nodes = append(nodes, node)
	}
	return nodes, nil
}

// cardTimelineView builds a card detail page's timeline read-model view:
// CardPinnedItems plus one BuildCardTimeline page (starting at cursor),
// resolved to display form (child project ids -> names) and enriched with
// the pinned dispatched child's awaiting-question link, which neither read
// model resolves on its own.
func (h *WebHandler) cardTimelineView(cardID, cursor string) (*templates.CardTimelineView, error) {
	if h.CardTimeline == nil {
		return &templates.CardTimelineView{}, nil
	}
	tl, err := h.cardPinnedView(cardID)
	if err != nil {
		return nil, err
	}
	page, err := h.CardTimeline.BuildCardTimeline(cardID, cursor, 0)
	if err != nil {
		return nil, err
	}
	h.resolveCardItemChildProjects(page.Items)
	tl.History = page.Items
	tl.HasMore = page.HasMore
	tl.NextCursor = page.NextCursor
	return tl, nil
}

// cardPinnedView is cardTimelineView's lighter counterpart: pinned items
// only, skipping the BuildCardTimeline history scan a caller that only
// needs Pinned/AwaitingQuestionID would otherwise pay for on every call.
func (h *WebHandler) cardPinnedView(cardID string) (*templates.CardTimelineView, error) {
	if h.CardTimeline == nil {
		return &templates.CardTimelineView{}, nil
	}
	pinned, err := h.CardTimeline.CardPinnedItems(cardID)
	if err != nil {
		return nil, err
	}
	h.resolveCardItemChildProjects(pinned)
	detail, err := h.Service.GetTaskDetail(cardID)
	if err != nil {
		return nil, err
	}
	if detail == nil || detail.Task == nil {
		return nil, errors.New("current card unavailable")
	}
	attrs := map[string]*orchestrator.CardAttrs{cardID: h.loadTriage(cardID)}
	activity := h.cardActivityStates([]*orchestrator.Task{detail.Task}, attrs)[cardID]
	return &templates.CardTimelineView{
		Activity:           activity,
		Pinned:             pinned,
		AwaitingQuestionID: h.enrichPinnedChildLiveStatus(pinned),
	}, nil
}

// resolveCardItemChildProjects rewrites every CardItemChild's Spec.Project
// from a raw boid project id to its display name, in place — a raw id tells
// the reader nothing about where pressing Go would run the child. Falls
// back to the raw id (leaves the item unchanged) when the project can't be
// resolved (removed project, typo'd id): blanking it would hide where the
// child would run, the exact question this field exists to answer.
func (h *WebHandler) resolveCardItemChildProjects(items []timeline.CardItem) {
	for i := range items {
		it := &items[i]
		if it.Kind != timeline.CardItemChild || it.Child == nil || it.Child.Spec == nil || it.Child.Spec.Project == "" {
			continue
		}
		name := h.lookupProjectName(it.Child.Spec.Project)
		if name == "" {
			continue
		}
		specCopy := *it.Child.Spec
		specCopy.Project = name
		childCopy := *it.Child
		childCopy.Spec = &specCopy
		it.Child = &childCopy
	}
}

// enrichPinnedChildLiveStatus resolves the sole pinned dispatched child's
// live task status (for its chip) and, when that status is awaiting, its
// open question id (for a direct-to-question link). Mutates pinned in
// place via a fresh copy per child.
func (h *WebHandler) enrichPinnedChildLiveStatus(pinned []timeline.CardItem) string {
	for i := range pinned {
		it := &pinned[i]
		if it.Kind != timeline.CardItemChild || it.Child == nil {
			continue
		}
		c := it.Child
		if c.Status != orchestrator.TaskTriageChildStatusDispatched || !c.TaskExists || c.TaskRef == "" {
			continue
		}
		detail, err := h.Service.GetTaskDetail(c.TaskRef)
		if err != nil || detail == nil || detail.Task == nil {
			continue
		}
		childCopy := *c
		childCopy.LiveStatus = string(detail.Task.Status)
		it.Child = &childCopy
		if detail.Task.Status == orchestrator.TaskStatusAwaiting && detail.Task.Exec != nil {
			if qid := orchestrator.GetAwaitingPayload(detail.Task.Exec.Payload).QuestionID; qid != "" {
				return qid
			}
		}
	}
	return ""
}

// loadTriage is the best-effort task_triage sidecar lookup the card detail
// page's current-summary read uses: a missing sidecar row (nil TaskTriage
// store, no row, lookup error) is not fatal and simply returns nil.
func (h *WebHandler) loadTriage(id string) *orchestrator.CardAttrs {
	if h.TaskTriage == nil {
		return nil
	}
	triage, err := h.TaskTriage.GetTaskTriage(id)
	if err != nil {
		return nil
	}
	return triage
}

// lookupProjectName resolves a project ID to its display name (Meta.Name),
// returning "" when the project or name is missing.
func (h *WebHandler) lookupProjectName(projectID string) string {
	if projectID == "" {
		return ""
	}
	projects, err := h.Service.ListProjects()
	if err != nil {
		return ""
	}
	for _, p := range projects {
		if p.ID == projectID {
			return p.Meta.Name
		}
	}
	return ""
}

// TaskDetailFragment returns a partial HTML fragment for the task detail page.
// The `kind` query parameter selects which section to render:
//   - "timeline": action history section (shared by both entity layouts)
//   - "status":   the meta strip — card or execution variant, by task.Type
//   - "pinned":   a card's pinned items only (#task-pinned) — no-op (empty
//     body) for an execution task, which has no such section
func (h *WebHandler) TaskDetailFragment(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	kind := r.URL.Query().Get("kind")
	switch kind {
	case "timeline":
		childTree, childErr := h.execChildTree(detail.Task)
		if childErr != nil {
			http.Error(w, "Unable to load subtasks", http.StatusInternalServerError)
			return
		}
		templates.TaskDetailTimelineSection(detail.Task, detailTimelineGroups(detail), childTree).Render(r.Context(), w)
	case "status":
		projectName := h.lookupProjectName(detail.Task.ProjectID)
		if detail.Task.Type == orchestrator.TaskTypeCard {
			var summary string
			if triage := h.loadTriage(id); triage != nil {
				summary = templates.TriageSummary(triage.Detail)
			}
			templates.TaskDetailCardStatusSection(detail.Task, "", projectName, summary, detail.Identities).Render(r.Context(), w)
		} else {
			templates.TaskDetailExecStatusSection(detail.Task, "", projectName, nil, detail.Identities).Render(r.Context(), w)
		}
	case "pinned":
		if detail.Task.Type != orchestrator.TaskTypeCard {
			return
		}
		tl, tlErr := h.cardPinnedView(id)
		if tlErr != nil {
			slog.Warn("card pinned view failed", "task_id", id, "error", tlErr)
			http.Error(w, "Unable to load current activity", http.StatusInternalServerError)
			return
		}
		templates.TaskDetailCardPinnedSection(tl, detail.Task.ID, detail.Task.Status).Render(r.Context(), w)
	case "operations":
		if detail.Task.Type != orchestrator.TaskTypeCard {
			return
		}
		if h.OperationResults == nil {
			http.Error(w, "operation results unavailable", http.StatusServiceUnavailable)
			return
		}
		results, loadErr := h.OperationResults.ListOperationResults(id, 20)
		if loadErr != nil {
			http.Error(w, "failed to load operation results", http.StatusInternalServerError)
			return
		}
		if selectedID := r.URL.Query().Get("operation"); selectedID != "" {
			selected, getErr := h.OperationResults.GetOperationResult(id, selectedID)
			if errors.Is(getErr, sql.ErrNoRows) {
				http.Error(w, "operation result not found", http.StatusNotFound)
				return
			}
			if getErr != nil {
				http.Error(w, "failed to load operation result", http.StatusInternalServerError)
				return
			}
			results = promoteOperationResult(results, selected)
		}
		templates.OperationResultsSection(results).Render(r.Context(), w)
	default:
		http.Error(w, "unknown fragment kind", http.StatusBadRequest)
	}
}

// TaskCardTimelineOlder returns the next page of a card's timeline history
// as an HTML fragment ("Load older"): the older items plus a fresh
// Load-older control, or none once exhausted. Appended by the client
// (hx-swap="outerHTML" on the button that requested it), never replacing
// what is already on the page, so earlier pages already loaded survive.
func (h *WebHandler) TaskCardTimelineOlder(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil || detail.Task == nil || detail.Task.Type != orchestrator.TaskTypeCard {
		http.Error(w, "card not found", http.StatusNotFound)
		return
	}
	if h.CardTimeline == nil {
		http.Error(w, "card timeline not available", http.StatusNotFound)
		return
	}
	cursor := r.URL.Query().Get("cursor")
	lastDate := r.URL.Query().Get("last_date")
	page, err := h.CardTimeline.BuildCardTimeline(id, cursor, 0)
	if err != nil {
		http.Error(w, "failed to load timeline", http.StatusInternalServerError)
		return
	}
	h.resolveCardItemChildProjects(page.Items)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.CardHistoryOlderFragment(id, page.Items, page.HasMore, page.NextCursor, lastDate).Render(r.Context(), w)
}

// cardTimelineHeadMaxPages bounds TaskCardTimelineHead's internal re-scan —
// a safety valve against a runaway loop on corrupt/foreign frontier input,
// not a limit any real card timeline is expected to hit (each page already
// holds timeline.MaxCardTimelineLimit items).
const cardTimelineHeadMaxPages = 50

// TaskCardTimelineHead re-renders a card's history from the top down
// through (and including) the item at frontier, as a flat item list with no
// Load-older control — lets a caller safely replace its own already-loaded
// head range in place, since an item excluded from that range when it was
// first loaded can later reappear anywhere within it. frontier empty means
// walk until history is exhausted.
func (h *WebHandler) TaskCardTimelineHead(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil || detail.Task == nil || detail.Task.Type != orchestrator.TaskTypeCard {
		http.Error(w, "card not found", http.StatusNotFound)
		return
	}
	if h.CardTimeline == nil {
		http.Error(w, "card timeline not available", http.StatusNotFound)
		return
	}

	frontier := r.URL.Query().Get("frontier")
	var frontierTime time.Time
	var frontierID string
	if frontier != "" {
		frontierTime, frontierID, err = orchestrator.DecodeActionCursor(frontier)
		if err != nil {
			http.Error(w, "invalid frontier cursor", http.StatusBadRequest)
			return
		}
	}

	var items []timeline.CardItem
	cursor := ""
	for i := 0; i < cardTimelineHeadMaxPages; i++ {
		page, perr := h.CardTimeline.BuildCardTimeline(id, cursor, timeline.MaxCardTimelineLimit)
		if perr != nil {
			http.Error(w, "failed to load timeline", http.StatusInternalServerError)
			return
		}
		reachedFrontier := false
		for _, it := range page.Items {
			items = append(items, it)
			if frontier != "" && it.HasTime && it.ID == frontierID && it.Time.Equal(frontierTime) {
				reachedFrontier = true
				break
			}
		}
		if reachedFrontier || !page.HasMore {
			break
		}
		cursor = page.NextCursor
	}

	h.resolveCardItemChildProjects(items)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.CardHistoryHeadFragment(id, items).Render(r.Context(), w)
}

func (h *WebHandler) PostAction(w http.ResponseWriter, r *http.Request) {
	receivedAt := time.Now().UTC()
	id := chi.URLParam(r, "id")
	actionType := r.FormValue("type")
	if actionType == "" {
		redirectTaskErr(w, r, id, errors.New("type is required"))
		return
	}
	var pending *orchestrator.OperationResult
	if actionType == "go" {
		pending = pendingOperation(id, "go", "Go", receivedAt)
		if err := h.createPendingOperation(pending); err != nil {
			h.renderOperationUnavailable(w, r, id, "", false)
			return
		}
	}
	var application *ActionApplication
	var err error
	if service, ok := h.Service.(interface {
		ApplyActionWithResult(string, string) (*ActionApplication, error)
	}); ok {
		application, err = service.ApplyActionWithResult(id, actionType)
	} else {
		err = h.Service.ApplyAction(id, actionType)
	}
	if actionType == "go" {
		outcome := completedGoOperation(id, "Go", application, err)
		if updateErr := h.finalizeOperation(pending, outcome); updateErr != nil {
			pending.Notice = operationReceiptUpdateWarning
			h.renderOperationState(w, r, id, statusCodeForOperationError(err), "", pending)
			return
		}
		redirectOperationResult(w, r, id, pending.ID, err)
		return
	}
	if err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	redirectTask(w, r, id)
}

// PostAnswerSuggestion handles the task detail page's Accept/Reject buttons
// on a task_triage suggestion card. answer is required ("accept"/"reject" —
// validated downstream by answeredPayload); verb/basis are optional and
// forwarded verbatim from hidden form fields populated from the suggestion
// currently shown.
func (h *WebHandler) PostAnswerSuggestion(w http.ResponseWriter, r *http.Request) {
	receivedAt := time.Now().UTC()
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	answer := r.FormValue("answer")
	if answer == "" {
		redirectTaskErr(w, r, id, errors.New("answer is required"))
		return
	}
	req := AnswerSuggestionRequest{
		Answer: answer,
		Verb:   r.FormValue("verb"),
		Basis:  r.FormValue("basis"),
	}
	var pending *orchestrator.OperationResult
	if answer == "accept" && req.Verb == "go" {
		pending = pendingOperation(id, "go", "Go (accepted suggestion)", receivedAt)
		if err := h.createPendingOperation(pending); err != nil {
			h.renderOperationUnavailable(w, r, id, "", false)
			return
		}
	}
	var application *ActionApplication
	var err error
	if service, ok := h.Service.(interface {
		AnswerSuggestionWithResult(string, AnswerSuggestionRequest) (*ActionApplication, error)
	}); ok {
		application, err = service.AnswerSuggestionWithResult(id, req)
	} else {
		err = h.Service.AnswerSuggestion(id, req)
	}
	if answer == "accept" && req.Verb == "go" {
		outcome := completedGoOperation(id, "Go (accepted suggestion)", application, err)
		if updateErr := h.finalizeOperation(pending, outcome); updateErr != nil {
			pending.Notice = operationReceiptUpdateWarning
			h.renderOperationState(w, r, id, statusCodeForOperationError(err), "", pending)
			return
		}
		redirectOperationResult(w, r, id, pending.ID, err)
		return
	}
	if err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	redirectTask(w, r, id)
}

const (
	operationReceiptUnavailableMessage = "Operation receipts are temporarily unavailable. Nothing was submitted. Refresh and try again."
	operationReceiptUpdateWarning      = "This outcome could not be saved. Reloading will show the outcome as unknown."
)

func pendingOperation(taskID, operationType, label string, receivedAt time.Time) *orchestrator.OperationResult {
	return &orchestrator.OperationResult{
		ID: uuid.NewString(), TaskID: taskID, OperationType: operationType, OperationLabel: label,
		Result: orchestrator.OperationResultUnknown, ReasonCode: orchestrator.OperationReasonOutcomePending,
		CreatedAt: receivedAt,
	}
}

func (h *WebHandler) createPendingOperation(result *orchestrator.OperationResult) error {
	if h.OperationResults == nil {
		return errors.New("operation result store is unavailable")
	}
	if err := h.OperationResults.CreateOperationResult(result); err != nil {
		slog.Error("create pending UI operation result failed", "task_id", result.TaskID, "operation", result.OperationType, "error", err)
		return err
	}
	return nil
}

func (h *WebHandler) finalizeOperation(pending, outcome *orchestrator.OperationResult) error {
	pending.OperationLabel = outcome.OperationLabel
	pending.Result = outcome.Result
	pending.ReasonCode = outcome.ReasonCode
	pending.TargetTaskID = outcome.TargetTaskID
	pending.TargetSessionID = outcome.TargetSessionID
	pending.TargetRequestID = outcome.TargetRequestID
	if err := h.OperationResults.UpdateOperationResult(pending); err != nil {
		slog.Error("finalize UI operation result failed", "task_id", pending.TaskID, "operation", pending.OperationType, "operation_id", pending.ID, "error", err)
		return err
	}
	return nil
}

func (h *WebHandler) renderOperationUnavailable(w http.ResponseWriter, r *http.Request, id, instruction string, command bool) {
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	var form *templates.CardCommandFormState
	if command {
		form = &templates.CardCommandFormState{Instruction: instruction}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Boid-Operation-Status", "not-submitted")
	w.WriteHeader(http.StatusServiceUnavailable)
	h.renderTaskDetailPage(w, r, detail, "timeline", operationReceiptUnavailableMessage, form, nil)
}

func completedGoOperation(taskID, label string, application *ActionApplication, err error) *orchestrator.OperationResult {
	if err != nil {
		op := operationRejection(taskID, "go", label, err)
		if application != nil && application.DecisionAccepted {
			if application.TargetTaskID != "" {
				op.TargetTaskID = application.TargetTaskID
			}
			if op.Result == orchestrator.OperationResultUnknown {
				op.ReasonCode = orchestrator.OperationReasonSuggestionAcceptedLaunchUnknown
			} else {
				op.Result = orchestrator.OperationResultAccepted
				op.ReasonCode = orchestrator.OperationReasonSuggestionAcceptedLaunchRejected
			}
		}
		return op
	}
	op := &orchestrator.OperationResult{TaskID: taskID, OperationType: "go", OperationLabel: label,
		Result: orchestrator.OperationResultAccepted, ReasonCode: orchestrator.OperationReasonRequestAccepted}
	if application != nil && application.TargetTaskID != "" {
		op.TargetTaskID = application.TargetTaskID
		op.Result = orchestrator.OperationResultStarted
		op.ReasonCode = orchestrator.OperationReasonExecutionStarted
	}
	return op
}

func operationRejection(taskID, operationType, label string, err error) *orchestrator.OperationResult {
	op := &orchestrator.OperationResult{TaskID: taskID, OperationType: operationType, OperationLabel: label,
		Result: orchestrator.OperationResultRejected, ReasonCode: operationReasonForError(err)}
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		op.TargetRequestID = statusErr.TargetRequestID
		op.TargetTaskID = statusErr.TargetTaskID
		if statusErr.TargetKind == orchestrator.CardRequestTargetKindTask {
			op.TargetTaskID = statusErr.TargetID
		}
		if statusErr.TargetKind == orchestrator.CardRequestTargetKindSession {
			op.TargetSessionID = statusErr.TargetID
		}
	}
	if op.ReasonCode == orchestrator.OperationReasonInternalError {
		op.Result = orchestrator.OperationResultUnknown
	}
	return op
}

func redirectOperationResult(w http.ResponseWriter, r *http.Request, id, operationID string, _ error) {
	values := url.Values{"operation": {operationID}}
	http.Redirect(w, r, "/tasks/"+id+"?"+values.Encode(), http.StatusSeeOther)
}

func (h *WebHandler) renderOperationState(w http.ResponseWriter, r *http.Request, id string, code int, message string, op *orchestrator.OperationResult) {
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Boid-Operation-ID", op.ID)
	w.WriteHeader(code)
	h.renderTaskDetailPage(w, r, detail, "timeline", message, nil, op)
}

func statusCodeForOperationError(err error) int {
	if err == nil {
		return http.StatusOK
	}
	var statusErr *StatusError
	if errors.As(err, &statusErr) {
		return statusErr.Code
	}
	return http.StatusInternalServerError
}

// operationReasonForError maps typed status metadata, never message text,
// to the stable reason vocabulary stored for UI operation history.
func operationReasonForError(err error) string {
	var statusErr *StatusError
	if errors.As(err, &statusErr) && statusErr.OperationReason != "" {
		return statusErr.OperationReason
	}
	switch statusCodeForOperationError(err) {
	case http.StatusBadRequest:
		return orchestrator.OperationReasonInvalidRequest
	case http.StatusNotFound:
		return orchestrator.OperationReasonNotFound
	case http.StatusConflict:
		return orchestrator.OperationReasonConflict
	default:
		return orchestrator.OperationReasonInternalError
	}
}

func (h *WebHandler) GetTaskEdit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}
	if detail.Task.Status != orchestrator.TaskStatusPending {
		redirectTask(w, r, id)
		return
	}
	projects, _ := h.Service.ListProjects()
	errorMsg := r.URL.Query().Get("error")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskEditPage(detail.Task, projects, errorMsg).Render(r.Context(), w)
}

func (h *WebHandler) PostEdit(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/tasks/"+id+"/edit?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Redirect(w, r, "/tasks/"+id+"/edit?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	title := strings.TrimSpace(r.FormValue("title"))
	projectID := strings.TrimSpace(r.FormValue("project_id"))
	description := r.FormValue("description")
	message := r.FormValue("message")
	model := strings.TrimSpace(r.FormValue("model"))
	agent := strings.TrimSpace(r.FormValue("agent"))

	// Instructions is execution-only; safe to read unconditionally here
	// because GetTaskEdit only ever renders this form for a task in
	// "pending" status, which is itself execution-only — a card can never
	// reach this handler. execInsts stays nil (not a panic) if that
	// invariant is somehow violated.
	var execInsts orchestrator.Instructions
	if detail.Task.Exec != nil {
		execInsts = detail.Task.Exec.Instructions
	}
	insts := execInsts
	if len(insts) == 0 {
		insts = orchestrator.Instructions{{
			Agent:   agent,
			Message: message,
			Model:   model,
		}}
	} else {
		clone := make(orchestrator.Instructions, len(insts))
		copy(clone, insts)
		active := clone[len(clone)-1]
		active.Message = message
		active.Model = model
		active.Agent = agent
		clone[len(clone)-1] = active
		insts = clone
	}

	instsJSON, err := json.Marshal(insts)
	if err != nil {
		http.Redirect(w, r, "/tasks/"+id+"/edit?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}

	remoteID := r.FormValue("remote_id")
	req := UpdateTaskRequest{
		Title:        title,
		ProjectID:    projectID,
		Description:  description,
		RemoteID:     &remoteID,
		Instructions: json.RawMessage(instsJSON),
	}

	target := "/tasks/" + id
	if err := h.Service.UpdateTask(r.Context(), id, req); err != nil {
		target = "/tasks/" + id + "/edit?error=" + url.QueryEscape(err.Error())
	}

	redirectOrHXRedirect(w, r, target)
}

func (h *WebHandler) PostDuplicate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	newID, err := h.Service.DuplicateTask(r.Context(), id)
	if err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	redirectTask(w, r, newID)
}

func (h *WebHandler) PostRerun(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Service.RerunTask(id, RerunTaskRequest{}); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	redirectTask(w, r, id)
}

func (h *WebHandler) ReopenForm(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	if detail.Task.Status != orchestrator.TaskStatusDone && detail.Task.Status != orchestrator.TaskStatusAborted && detail.Task.Status != orchestrator.TaskStatusDropped {
		// dropped→parked is a second reopen edge for cards, alongside the
		// execution machine's own done/aborted→executing.
		redirectTaskErr(w, r, id, errors.New("reopen is only available for done, aborted, or dropped tasks"))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.TaskReopen(detail.Task).Render(r.Context(), w)
}

func (h *WebHandler) PostReopen(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := r.ParseForm(); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	message := strings.TrimSpace(r.FormValue("message"))
	if err := h.Service.ReopenTask(id, ReopenTaskRequest{Message: message}); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	redirectTask(w, r, id)
}

// QuestionPage renders the dedicated Q&A turn page at
// `/tasks/{id}/questions/{question_id}`. The notification deep-link from
// `boid task notify --ask` lands here. The page shows the question and either
// an answer form (when this is the active awaiting turn) or the recorded
// answer (when an answer action exists for the same question_id).
func (h *WebHandler) QuestionPage(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	questionID := chi.URLParam(r, "question_id")
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil {
		http.Error(w, "Task not found", http.StatusNotFound)
		return
	}

	var (
		question string
		answer   string
		found    bool
	)
	for _, a := range detail.Actions {
		ap := orchestrator.GetAwaitingPayload(a.Payload)
		if ap.QuestionID != questionID {
			continue
		}
		switch a.Type {
		case "ask":
			question = ap.Question
			found = true
		case "answer":
			if ap.PendingAnswer != "" {
				answer = ap.PendingAnswer
			}
		}
	}
	if !found {
		http.Error(w, "Question not found", http.StatusNotFound)
		return
	}

	// Payload/awaiting is execution-only — this page only exists for tasks
	// that went through an "ask" action, which only ever happens on an
	// execution task, so detail.Task.Exec is expected non-nil here.
	var taskPayload json.RawMessage
	if detail.Task.Exec != nil {
		taskPayload = detail.Task.Exec.Payload
	}
	currentAwaiting := orchestrator.GetAwaitingPayload(taskPayload)
	isActive := detail.Task.Status == orchestrator.TaskStatusAwaiting && currentAwaiting.QuestionID == questionID

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.QuestionPage(detail.Task, templates.QuestionTurn{
		QuestionID: questionID,
		Question:   question,
		Answer:     answer,
		IsActive:   isActive,
		WasAborted: detail.Task.Status == orchestrator.TaskStatusAborted && answer == "",
	}).Render(r.Context(), w)
}

func (h *WebHandler) PostAnswer(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := parseTaskForm(r); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	questionID := r.FormValue("question_id")
	answer := strings.TrimSpace(r.FormValue("answer"))

	// Validate + persist attachments before submitting the answer so the
	// running agent observes a consistent view: any file referenced in the
	// answer text (via the `[attachment: <name>]` marker inserted by the
	// paste-attach JS) must already be on disk by the time AnswerTask wakes
	// up the task, so a `boid task attachments get <name>` call issued right
	// after the wake-up finds it immediately.
	uploads := taskFormAttachments(r)
	if len(uploads) > 0 {
		if h.AttachmentsRoot == "" {
			redirectTaskErr(w, r, id, errors.New("attachments root not configured"))
			return
		}
		if err := ValidateAttachmentHeaders(uploads); err != nil {
			redirectTaskErr(w, r, id, err)
			return
		}
		if _, err := SaveMultipartAttachments(h.AttachmentsRoot, id, uploads); err != nil {
			redirectTaskErr(w, r, id, fmt.Errorf("attachment save failed: %w", err))
			return
		}
	}

	target := "/tasks/" + id
	if err := h.Service.AnswerTask(orchestrator.WithActor(r.Context(), orchestrator.ActorHuman), id, questionID, answer); err != nil {
		target = "/tasks/" + id + "?error=" + url.QueryEscape(err.Error())
	}
	redirectOrHXRedirect(w, r, target)
}

// PostDelete deletes the task and redirects to the task list.
// Errors are surfaced via ?error= on the same task page so the user sees the
// reason (e.g. dependents exist).
func (h *WebHandler) PostDelete(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := h.Service.DeleteTask(id, false); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *WebHandler) HookReplayList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	status := r.URL.Query().Get("status")
	hooks, err := h.Service.ListHooksForStatus(id, status)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	errorMsg := r.URL.Query().Get("error")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.HookReplayList(id, status, hooks, errorMsg).Render(r.Context(), w)
}

func (h *WebHandler) PostHookReplay(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	hookID, err := url.PathUnescape(chi.URLParam(r, "hook_id"))
	if err != nil {
		http.Error(w, "invalid hook id", http.StatusBadRequest)
		return
	}
	_, err = h.Service.ReplayHook(r.Context(), id, ReplayHookRequest{HookID: hookID})
	if err != nil {
		http.Redirect(w, r, "/tasks/"+id+"/hooks?error="+url.QueryEscape(err.Error()), http.StatusSeeOther)
		return
	}
	redirectTask(w, r, id)
}

func (h *WebHandler) JobDetail(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	job, err := h.Service.GetJob(id)
	if err != nil {
		http.Error(w, "Job not found", http.StatusNotFound)
		return
	}
	hookID := ""
	if job.Role == "hook" {
		hookID = job.HandlerID
	}
	view := &templates.JobContextView{
		ID:          job.ID,
		TaskID:      job.TaskID,
		ProjectID:   job.ProjectID,
		TaskTitle:   job.TaskTitle,
		HandlerID:   job.HandlerID,
		DisplayName: job.DisplayName,
		Role:        job.Role,
		HookID:      hookID,
		Status:      string(job.Status),
		ExitCode:    job.ExitCode,
		Interactive: job.Interactive,
		CreatedAt:   job.CreatedAt,
		UpdatedAt:   job.UpdatedAt,
		Output:      job.Output,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.JobDetail(view).Render(r.Context(), w)
}

// JobTerminal redirects legacy deep-links (/jobs/{id}/terminal) to the job
// detail page (/jobs/{id}), which now renders the terminal inline.
func (h *WebHandler) JobTerminal(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	http.Redirect(w, r, "/jobs/"+id, http.StatusFound)
}

// PostStartSession launches a HarnessAdapter-backed session for the project
// from the Web UI's [New Session] dialog.
func (h *WebHandler) PostStartSession(w http.ResponseWriter, r *http.Request) {
	if h.SessionDispatcher == nil {
		http.Error(w, "session dispatcher not wired", http.StatusNotImplemented)
		return
	}
	projectID := chi.URLParam(r, "id")
	_ = r.ParseForm()
	req := StartSessionRequest{
		ProjectID:   projectID,
		HarnessType: strings.TrimSpace(r.FormValue("harness_type")),
		Model:       strings.TrimSpace(r.FormValue("model")),
		Instruction: strings.TrimSpace(r.FormValue("instruction")),
		Readonly:    r.FormValue("readonly") == "on",
		DisplayName: strings.TrimSpace(r.FormValue("name")),
	}
	if msg := ValidateHarnessType(req.HarnessType); msg != "" {
		backURL := "/sessions/new?project=" + url.QueryEscape(projectID) + "&error=" + url.QueryEscape(msg)
		http.Redirect(w, r, backURL, http.StatusSeeOther)
		return
	}
	result, err := h.SessionDispatcher.StartSession(r.Context(), req)
	if err != nil {
		backURL := "/sessions/new?project=" + url.QueryEscape(projectID) + "&error=" + url.QueryEscape(err.Error())
		http.Redirect(w, r, backURL, http.StatusSeeOther)
		return
	}
	jobURL := "/jobs/" + result.JobID
	redirectOrHXRedirect(w, r, jobURL)
}

// WebManagementHandler serves the CLI management API at /api/web/*.
// All routes are accessible only via UNIX socket (CLI control plane).
// Pairer issues pairing codes.
type Pairer interface {
	Issue(ctx context.Context, label string) (string, error)
}

type WebManagementHandler struct {
	Pairing   Pairer
	Store     *auth.Store
	PublicURL string
	Registry  *auth.ConnectionRegistry
}

func (h *WebManagementHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Post("/pair", h.PostPair)
	r.Get("/devices", h.GetDevices)
	r.Delete("/devices/{id}", h.DeleteDevice)
	r.Delete("/devices", h.DeleteAllDevices)
	return r
}

type pairResponse struct {
	Code      string `json:"code"`
	URL       string `json:"url,omitempty"`
	ExpiresIn int    `json:"expires_in"`
}

func (h *WebManagementHandler) PostPair(w http.ResponseWriter, r *http.Request) {
	var req auth.PairRequest
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&req) // label is optional
	}
	code, err := h.Pairing.Issue(r.Context(), req.Label)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := pairResponse{
		Code:      code,
		ExpiresIn: 300,
	}
	if h.PublicURL != "" {
		resp.URL = h.PublicURL + "/auth?token=" + url.QueryEscape(code)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp) // best-effort; client may have disconnected
}

type deviceResponse struct {
	ID         string `json:"id"`
	Label      string `json:"label,omitempty"`
	CreatedAt  string `json:"created_at"`
	LastSeenAt string `json:"last_seen_at"`
}

func (h *WebManagementHandler) GetDevices(w http.ResponseWriter, r *http.Request) {
	devices, err := h.Store.ListDevices(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	resp := make([]deviceResponse, 0, len(devices))
	for _, d := range devices {
		if d.RevokedAt != nil {
			continue
		}
		resp = append(resp, deviceResponse{
			ID:         d.ID,
			Label:      d.Label,
			CreatedAt:  d.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
			LastSeenAt: d.LastSeenAt.UTC().Format("2006-01-02T15:04:05Z"),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp) // best-effort; client may have disconnected
}

func (h *WebManagementHandler) DeleteDevice(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	err := h.Store.RevokeDevice(r.Context(), id)
	if errors.Is(err, auth.ErrDeviceNotFound) {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.Registry != nil {
		h.Registry.RevokeDevice(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *WebManagementHandler) DeleteAllDevices(w http.ResponseWriter, r *http.Request) {
	if err := h.Store.RevokeAllDevices(r.Context()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if h.Registry != nil {
		h.Registry.RevokeAll()
	}
	w.WriteHeader(http.StatusNoContent)
}

// loginPairing redeems a one-time pairing code.
type loginPairing interface {
	Redeem(ctx context.Context, code string) (string, error)
}

// loginSigner issues a session cookie.
type loginSigner interface {
	Issue(w http.ResponseWriter, deviceID string) error
}

// loginDeviceStore persists a new device after successful pairing.
type loginDeviceStore interface {
	InsertDevice(ctx context.Context, id, label string, cookieHash []byte) error
}

// loginRateLimiter guards against brute-force attempts.
type loginRateLimiter interface {
	Allowed(ip string) bool
	RecordFailure(ip string)
}

// LoginHandler handles /login and /auth.
type LoginHandler struct {
	Pairing loginPairing
	Signer  loginSigner
	Store   loginDeviceStore
	Limiter loginRateLimiter
}

// redeemErrorKey maps a Pairing.Redeem failure onto a short, stable key that
// travels in the ?error= query param and is turned back into prose by
// loginErrorMessage. The three redeem sentinels have genuinely different
// remedies (wait for a new code / re-issue / retype), so they are kept
// distinguishable rather than collapsed into one message.
func redeemErrorKey(err error) string {
	switch {
	case errors.Is(err, auth.ErrCodeExpired):
		return "expired"
	case errors.Is(err, auth.ErrCodeConsumed):
		return "used"
	case errors.Is(err, auth.ErrCodeNotFound):
		return "invalid"
	default:
		return "error"
	}
}

// isRedeemClientFault reports whether the failure is the caller's fault (a
// wrong/stale code) rather than ours (DB/IO). Only the former may draw down
// the brute-force rate limit — mirroring DeviceAuthHandler.PostDevice, where
// double-punishing a server-side failure with a 15-minute IP lock would turn a
// transient SQLite hiccup into a lockout.
func isRedeemClientFault(err error) bool {
	return errors.Is(err, auth.ErrCodeExpired) ||
		errors.Is(err, auth.ErrCodeConsumed) ||
		errors.Is(err, auth.ErrCodeNotFound)
}

// loginErrorMessage turns an ?error= key into the sentence shown on the login
// page. Unknown keys collapse to a generic message on purpose: the value is
// caller-controlled, and echoing it back would put arbitrary text on the page.
func loginErrorMessage(key string) string {
	switch key {
	case "":
		return ""
	case "expired":
		return "ペアリングコードの有効期限 (5分) が切れています。`boid web pair` で新しいコードを発行してください。"
	case "used":
		return "このペアリングコードは使用済みです。コードは1回しか使えません。`boid web pair` で新しいコードを発行してください。"
	case "invalid":
		return "ペアリングコードが見つかりません。ハイフンを含めて入力してください (大文字・小文字は問いません)。"
	default:
		return "ログインできませんでした。`boid web pair` で新しいコードを発行してやり直してください。"
	}
}

func (h *LoginHandler) GetLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	templates.Login(loginErrorMessage(r.URL.Query().Get("error"))).Render(r.Context(), w)
}

func (h *LoginHandler) PostLogin(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if !h.Limiter.Allowed(ip) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	// The code is NOT normalized here: auth.HashCode does it on both the
	// issue and the redeem side, so every caller of Redeem gets the same
	// tolerance for case, hyphenation and stray whitespace.
	code := r.FormValue("code")
	label, err := h.Pairing.Redeem(r.Context(), code)
	if err != nil {
		if isRedeemClientFault(err) {
			h.Limiter.RecordFailure(ip)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		templates.Login(loginErrorMessage(redeemErrorKey(err))).Render(r.Context(), w)
		return
	}
	if err := h.issueSession(w, r, label); err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// GetAuth renders the confirmation page for a magic link / QR scan. It is
// deliberately side-effect free — it does NOT redeem the token, since GET
// requests get issued by browser preloading, QR-scanner previews, and
// in-app-browser prefetching, any of which would burn a single-use code
// before the human's real navigation. Consuming a one-time credential is a
// state change, so it belongs behind the POST that PostAuth serves.
func (h *LoginHandler) GetAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Nothing is validated yet — an expired or bogus token still renders the
	// page and fails on submit. That is intentional: validating here would
	// hand an unauthenticated prefetcher a code-probing oracle and let it
	// drain the rate limiter.
	templates.AuthConfirm(r.URL.Query().Get("token")).Render(r.Context(), w)
}

// PostAuth redeems the token carried by the confirmation page's form. This is
// the only path that spends a pairing code from the browser flow.
func (h *LoginHandler) PostAuth(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	if !h.Limiter.Allowed(ip) {
		http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
		return
	}
	token := r.FormValue("token")
	label, err := h.Pairing.Redeem(r.Context(), token)
	if err != nil {
		if isRedeemClientFault(err) {
			h.Limiter.RecordFailure(ip)
		}
		http.Redirect(w, r, "/login?error="+url.QueryEscape(redeemErrorKey(err)), http.StatusFound)
		return
	}
	if err := h.issueSession(w, r, label); err != nil {
		http.Redirect(w, r, "/login?error=error", http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// issueSession creates a new device row and sets the session cookie.
func (h *LoginHandler) issueSession(w http.ResponseWriter, r *http.Request, label string) error {
	if h.Signer == nil {
		return fmt.Errorf("session signer not configured")
	}
	deviceID := uuid.New().String()
	sum := sha256.Sum256([]byte(deviceID))
	if err := h.Store.InsertDevice(r.Context(), deviceID, label, sum[:]); err != nil {
		return err
	}
	return h.Signer.Issue(w, deviceID)
}

// remoteIP extracts the real client IP for per-client rate limiting.
// It checks proxy headers in order so that cloudflared and other reverse proxies
// get a fair per-client bucket. This is best-effort extraction, not spoof prevention.
func remoteIP(r *http.Request) string {
	// CF-Connecting-IP: set by Cloudflare edge, overwritten at ingress — most reliable.
	if cf := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); cf != "" {
		if ip := net.ParseIP(cf); ip != nil {
			return ip.String()
		}
	}
	// X-Forwarded-For: leftmost entry is the originating client.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
			return ip.String()
		}
	}
	// Fallback to the TCP peer address.
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
