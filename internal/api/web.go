package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"sort"
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
}

func (h *WebHandler) Routes() chi.Router {
	r := chi.NewRouter()
	r.Get("/", h.TaskList)
	r.Get("/tasks/new", h.TaskNew)
	r.Post("/tasks", h.PostTaskCreate)
	r.Get("/tasks/{id}", h.TaskDetail)
	r.Get("/tasks/{id}/fragment", h.TaskDetailFragment)
	r.Get("/tasks/{id}/edit", h.GetTaskEdit)
	r.Post("/tasks/{id}/edit", h.PostEdit)
	r.Post("/tasks/{id}/action", h.PostAction)
	r.Post("/tasks/{id}/suggestion", h.PostAnswerSuggestion)
	r.Post("/tasks/{id}/duplicate", h.PostDuplicate)
	r.Post("/tasks/{id}/rerun", h.PostRerun)
	r.Get("/tasks/{id}/reopen", h.ReopenForm)
	r.Post("/tasks/{id}/reopen", h.PostReopen)
	r.Post("/tasks/{id}/delete", h.PostDelete)
	r.Post("/tasks/{id}/shape", h.PostStartShapingSession)
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

	task, err := h.Service.CreateTask(req)
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
	items := templates.BuildListRows(tasks, projectNames, h.triageByTaskID(tasks))

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

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
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
		templates.TaskDetailExecTabsSection(detail.Task, timelineGroups, detail.AvailableActions, tab).Render(r.Context(), w)
		return
	}

	projectName := h.lookupProjectName(detail.Task.ProjectID)
	var children []templates.ChildRow
	var suggestion orchestrator.Suggestion
	var childTree []templates.ChildTreeNode
	if detail.Task.Type == orchestrator.TaskTypeCard {
		triage := h.loadTriage(id)
		children = h.cardChildrenFromTriage(id, triage)
		suggestion = suggestionOf(triage)
	} else {
		childTree = h.execChildTree(detail.Task)
	}
	templates.TaskDetail(detail.Task, timelineGroups, detail.AvailableActions, errorMsg, tab, projectName, children, suggestion, childTree).Render(r.Context(), w)
}

// maxChildTreeDepth caps app-side recursion in execChildTree/listDescendants
// as a defensive guard against a pathological parent_id cycle — Task.
// ParentID can be rewritten via UpdateTask, so an unbounded walk is not
// provably impossible, just never expected in practice. Far above any real
// supervisor tree depth.
const maxChildTreeDepth = 50

// execChildTree returns task's full descendant subtree, depth-first: a root
// (parent_id == "") execution task detail page's own child tree. Returns
// nil for anything that is not a root task — including a card, whose
// children get the integrated ledger view instead (cardChildrenFromTriage)
// — or a root task with no children.
//
// Traversal is app-side (repeated ListTasks-by-parent_id calls), not a SQL
// recursive CTE.
func (h *WebHandler) execChildTree(task *orchestrator.Task) []templates.ChildTreeNode {
	if task == nil || task.ParentID != "" {
		return nil
	}
	return h.listDescendants(task.ID, 1)
}

// listDescendants is execChildTree's recursive body: one ListTasks call per
// tree level, flattened depth-first into a single slice (ChildTreeNode.Depth
// carries the level so the template can indent without a nested shape).
func (h *WebHandler) listDescendants(parentID string, depth int) []templates.ChildTreeNode {
	if depth > maxChildTreeDepth {
		return nil
	}
	kids, err := h.Service.ListTasks(orchestrator.TaskFilter{ParentID: &parentID})
	if err != nil || len(kids) == 0 {
		return nil
	}
	nodes := make([]templates.ChildTreeNode, 0, len(kids))
	for _, k := range kids {
		nodes = append(nodes, templates.ChildTreeNode{Task: k, Depth: depth})
		nodes = append(nodes, h.listDescendants(k.ID, depth+1)...)
	}
	return nodes
}

// cardChildrenFromTriage builds a card detail page's integrated child rows:
// the spec ledger (task_triage.detail.children, resolved to display form by
// resolveChildProjects) merged with the live status of each dispatched
// child's real task row. The live lookup is one parent_id query covering
// every child at once, not one query per ledger entry.
//
// A ledger entry only gets a LiveStatus when its own Status is "dispatched"
// AND its TaskRef resolves inside that query's result; otherwise
// ChildRow.DisplayStatus falls back to the ledger status with no error.
// When a lookup does succeed, the live status always wins over the bare
// "dispatched" ledger value.
func (h *WebHandler) cardChildrenFromTriage(cardID string, triage *orchestrator.CardAttrs) []templates.ChildRow {
	ledger := h.resolveChildProjects(childrenOf(triage))
	if len(ledger) == 0 {
		return nil
	}
	live := make(map[string]*orchestrator.Task, len(ledger))
	if kids, err := h.Service.ListTasks(orchestrator.TaskFilter{ParentID: &cardID}); err == nil {
		for _, k := range kids {
			live[k.ID] = k
		}
	}
	rows := make([]templates.ChildRow, 0, len(ledger))
	for _, c := range ledger {
		row := templates.ChildRow{Child: c}
		if c.Status == orchestrator.TaskTriageChildStatusDispatched && c.TaskRef != "" {
			if t, ok := live[c.TaskRef]; ok && t != nil {
				row.LiveStatus = string(t.Status)
				if t.Status == orchestrator.TaskStatusAwaiting && t.Exec != nil {
					row.AwaitingQuestionID = orchestrator.GetAwaitingPayload(t.Exec.Payload).QuestionID
				}
			}
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool { return childRowRank(rows[i]) < childRowRank(rows[j]) })
	return rows
}

// cardChildrenForDisplay is cardChildrenFromTriage plus its own triage load,
// for callers (TaskDetailFragment's "status" kind, hit on every SSE
// refresh) that don't already hold a loaded triage row.
func (h *WebHandler) cardChildrenForDisplay(cardID string) []templates.ChildRow {
	return h.cardChildrenFromTriage(cardID, h.loadTriage(cardID))
}

// childRowRank sorts by urgency (awaiting → executing → open/specced/other →
// closed) over ChildRow.DisplayStatus(). Every non-terminal status other
// than awaiting/executing shares the same middle tier.
func childRowRank(row templates.ChildRow) int {
	switch row.DisplayStatus() {
	case string(orchestrator.TaskStatusAwaiting):
		return 0
	case string(orchestrator.TaskStatusExecuting):
		return 1
	case string(orchestrator.TaskStatusDone), string(orchestrator.TaskStatusAborted), orchestrator.TaskTriageChildStatusClosed:
		return 3
	default:
		return 2
	}
}

// triageChildrenFor returns the task_triage.detail.children for id, or nil
// when there is no sidecar row / no CardStore wired / the detail blob
// doesn't parse. Best-effort — a task detail page must still render for a
// task with no triage children (almost every task).
func (h *WebHandler) triageChildrenFor(id string) []orchestrator.TaskTriageChild {
	return childrenOf(h.loadTriage(id))
}

// triageChildrenForDisplay is triageChildrenFor plus the one substitution the
// Web UI needs: a child's spec.Project is stored as a boid project id, which
// tells a reader nothing about where pressing Go would run the child, so
// this resolves it to the project name in a display-only copy — dispatch
// still reads the real id from the stored spec, never this copy.
func (h *WebHandler) triageChildrenForDisplay(id string) []orchestrator.TaskTriageChild {
	return h.resolveChildProjects(h.triageChildrenFor(id))
}

// resolveChildProjects is triageChildrenForDisplay's body, split out because
// the full-page render already holds the parsed triage row and would
// otherwise re-read it.
func (h *WebHandler) resolveChildProjects(children []orchestrator.TaskTriageChild) []orchestrator.TaskTriageChild {
	for i := range children {
		spec := children[i].Spec
		if spec == nil || spec.Project == "" {
			continue
		}
		name := h.lookupProjectName(spec.Project)
		if name == "" {
			// Removed project or a typo'd id: keep the raw value. Blanking it
			// would hide where the child would run, which is the exact
			// question this section exists to answer.
			continue
		}
		shown := *spec
		shown.Project = name
		children[i].Spec = &shown
	}
	return children
}

// triageSuggestionFor is triageChildrenFor's counterpart for
// task_triage.detail.suggestion — same best-effort posture (see suggestionOf).
func (h *WebHandler) triageSuggestionFor(id string) orchestrator.Suggestion {
	return suggestionOf(h.loadTriage(id))
}

// childrenOf parses a (possibly nil) task_triage row's Detail blob into its
// children list, or nil when the row is nil / has no Detail / the blob
// doesn't parse.
func childrenOf(triage *orchestrator.CardAttrs) []orchestrator.TaskTriageChild {
	if triage == nil || len(triage.Detail) == 0 {
		return nil
	}
	children, err := orchestrator.DetailChildren(triage.Detail)
	if err != nil {
		return nil
	}
	return children
}

// suggestionOf extracts task_triage.detail.suggestion (or detail.attrs.
// suggestion — orchestrator.DetailSuggestion) from a (possibly nil)
// task_triage row, mirroring childrenOf's best-effort posture: a missing
// row / empty detail / malformed suggestion blob all just return the zero
// Suggestion rather than erroring.
func suggestionOf(triage *orchestrator.CardAttrs) orchestrator.Suggestion {
	if triage == nil || len(triage.Detail) == 0 {
		return orchestrator.Suggestion{}
	}
	s, _ := orchestrator.DetailSuggestion(triage.Detail)
	return s
}

// loadTriage is the best-effort task_triage sidecar lookup shared by
// triageChildrenFor and PostStartShapingSession's working-status gate: a
// missing sidecar row (nil TaskTriage store, no row, lookup error) is not
// fatal and simply returns nil.
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
		templates.TaskDetailTimelineSection(detail.Task, detailTimelineGroups(detail)).Render(r.Context(), w)
	case "status":
		projectName := h.lookupProjectName(detail.Task.ProjectID)
		if detail.Task.Type == orchestrator.TaskTypeCard {
			children := h.cardChildrenForDisplay(id)
			suggestion := h.triageSuggestionFor(id)
			templates.TaskDetailCardStatusSection(detail.Task, "", projectName, children, suggestion).Render(r.Context(), w)
		} else {
			childTree := h.execChildTree(detail.Task)
			templates.TaskDetailExecStatusSection(detail.Task, "", projectName, childTree).Render(r.Context(), w)
		}
	default:
		http.Error(w, "unknown fragment kind", http.StatusBadRequest)
	}
}

func (h *WebHandler) PostAction(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	actionType := r.FormValue("type")
	if actionType == "" {
		redirectTaskErr(w, r, id, errors.New("type is required"))
		return
	}
	if err := h.Service.ApplyAction(id, actionType); err != nil {
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
	if err := h.Service.AnswerSuggestion(id, req); err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	redirectTask(w, r, id)
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
	if err := h.Service.UpdateTask(id, req); err != nil {
		target = "/tasks/" + id + "/edit?error=" + url.QueryEscape(err.Error())
	}

	redirectOrHXRedirect(w, r, target)
}

func (h *WebHandler) PostDuplicate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	newID, err := h.Service.DuplicateTask(id)
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
	if msg := validateHarnessType(req.HarnessType); msg != "" {
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

// PostStartShapingSession launches the "整形" (shaping) session for a triage
// card. Unlike PostStartSession (a blank session the operator configures by
// hand), this is a one-click launcher: the triage task's own project
// supplies the workspace context, and the card's id/title/description/kind/
// urgency are folded into the bootstrap instruction so the agent has the
// card in hand at turn one instead of the operator re-typing it.
//
// Reachable from a parked card OR a working card that has a task_triage
// sidecar row. working is included because a working triage task's
// children (task_triage.detail.children) keep needing shaping-session work
// after Go: an existing open child may need its spec defined, or a
// brand-new child may need to be added — the set of children is not fixed
// at dispatch time. Gated on the triage row existing (not just
// status=working) because most working tasks have no task_triage sidecar
// at all — without one there is no children list to add to or shape.
func (h *WebHandler) PostStartShapingSession(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if h.SessionDispatcher == nil {
		http.Error(w, "session dispatcher not wired", http.StatusNotImplemented)
		return
	}
	detail, err := h.Service.GetTaskDetail(id)
	if err != nil || detail == nil || detail.Task == nil {
		redirectTaskErr(w, r, id, fmt.Errorf("task not found: %s", id))
		return
	}
	task := detail.Task
	if task.Status != orchestrator.TaskStatusParked && task.Status != orchestrator.TaskStatusWorking {
		redirectTaskErr(w, r, id, fmt.Errorf("shaping session requires a parked or working task (status = %s)", task.Status))
		return
	}
	triage := h.loadTriage(id) // best-effort — a missing sidecar row is not fatal, see card_read.go's GetCard doc comment
	if task.Status == orchestrator.TaskStatusWorking && triage == nil {
		redirectTaskErr(w, r, id, fmt.Errorf("shaping session on a working task requires a task_triage sidecar row"))
		return
	}
	harnessType, model := shapingSessionDefaults(h.Service, task.ProjectID)
	req := StartSessionRequest{
		ProjectID:   task.ProjectID,
		HarnessType: harnessType,
		Model:       model,
		Instruction: buildShapingInstruction(task, triage),
		DisplayName: "Shape: " + task.Title,
	}
	result, err := h.SessionDispatcher.StartSession(r.Context(), req)
	if err != nil {
		redirectTaskErr(w, r, id, err)
		return
	}
	redirectOrHXRedirect(w, r, "/jobs/"+result.JobID)
}

// shapingSessionKey is the session_behaviors dictionary key the Shape
// button resolves (project.yaml session_behaviors.shape) — a free-naming
// dictionary scoped to sessions, unrelated to task_behaviors.
const shapingSessionKey = "shape"

// shapingSessionDefaults resolves the harness_type/model the Shape button
// should launch with, from the triage task's own (meta) project's
// project.yaml session_behaviors.shape entry. Falls back to ("claude", "")
// when the project can't be loaded, has no such entry, or the entry's
// harness_type is empty/invalid. The fallback is all-or-nothing (never
// harnessType=claude with the project's own model still attached): an
// invalid harness_type makes the whole entry untrustworthy, and forwarding
// just the model would launch claude with a model nobody chose for it.
func shapingSessionDefaults(svc WebService, projectID string) (harnessType, model string) {
	const fallbackHarness = "claude"
	project, err := svc.GetProjectByID(projectID)
	if err != nil || project == nil {
		return fallbackHarness, ""
	}
	behavior, ok := project.Meta.SessionBehaviors[shapingSessionKey]
	if !ok {
		return fallbackHarness, ""
	}
	if behavior.HarnessType == "" || validateHarnessType(behavior.HarnessType) != "" {
		slog.Warn("session_behaviors.shape.harness_type invalid; falling back to default",
			"project_id", projectID, "harness_type", behavior.HarnessType)
		return fallbackHarness, ""
	}
	return behavior.HarnessType, behavior.Model
}

// buildShapingInstruction folds a triage card's id/title/description/kind/
// urgency into the shaping session's bootstrap prompt. detail's opaque JSON
// is passed through verbatim rather than parsed here — the daemon does not
// interpret task_triage.detail's keys, and neither does this builder.
//
// Deliberately silent on HOW to write the card back: prescribing a
// procedure here can collide with a workspace's own write conventions. This
// function states only boid's own contract (record child_specced/
// child_added, never a card-level state transition — that is a human's
// accept or a suggest engine's job, never a shaping session's) and defers
// everything about "how" to the target project's own CLAUDE.md / skills.
func buildShapingInstruction(task *orchestrator.Task, triage *orchestrator.CardAttrs) string {
	var b strings.Builder
	if task.Status == orchestrator.TaskStatusWorking {
		b.WriteString("整形セッション: この working カードの子タスク一覧 (task_triage.detail.children) を編集してください。" +
			"以下の情報を参考に、既存の open な子タスクがあればそれぞれ対象 project・実行内容・完了条件を詰め、" +
			"対応が必要な作業を新たに見つけた場合は子タスクとして追加してください（必要なら Jira 起票を含む）。" +
			"子タスクが1件もない状態から追加を始めても構いません。\n\n")
	} else {
		b.WriteString("整形セッション: 以下の parked カードの内容を詰め、対象 project・実行内容・完了条件を確定してください。\n\n")
	}
	fmt.Fprintf(&b, "task_id: %s\n", task.ID)
	fmt.Fprintf(&b, "title: %s\n", task.Title)
	if triage != nil {
		if triage.Kind != "" {
			fmt.Fprintf(&b, "kind: %s\n", triage.Kind)
		}
		if triage.Urgency != "" {
			fmt.Fprintf(&b, "urgency: %s\n", triage.Urgency)
		}
	}
	b.WriteString("\n本文:\n")
	if task.Description != "" {
		b.WriteString(task.Description)
	} else {
		b.WriteString("(本文なし — summary のみで起票された card)")
	}
	if triage != nil && len(triage.Detail) > 0 && string(triage.Detail) != "{}" {
		b.WriteString("\n\ndetail (raw):\n")
		b.Write(triage.Detail)
	}
	if task.Status == orchestrator.TaskStatusWorking {
		b.WriteString("\n\n対話で子タスクの対象 project・実行内容・完了条件を固めたら、既存の子タスクは specced に更新し、" +
			"新たに追加した子タスクも同じ形で children に加えてください" +
			"（`child_specced` を打つだけです — このカード自身の状態遷移は行いません。working のまま子タスクの追加・整形だけを行います）。" +
			"card を進める・閉じる・戻す判断は行わないこと — それは人の accept、または khi の suggest 経由でのみ行われます" +
			"（card machine v2, docs/plans/suggestion-as-state-transition.md §3.2）。" +
			"更新の具体的な手順 (書き込み先・経路) はこの project 自身の CLAUDE.md やスキルに従うこと — " +
			"boid 側はここでは手順を指定しません。frontmatter やメタデータの直接編集が禁じられている project では、" +
			"それに従ってください。" +
			"整形の結果「やらない」と分かった子タスクについては、このセッションでは何もせず運用者に破棄の判断を委ねてください。")
	} else {
		b.WriteString("\n\n対話で対象 project・実行内容・完了条件を固めたら、既存の子タスクは specced に更新し、" +
			"新たに追加した子タスクも同じ形で children に加えてください" +
			"（`child_specced` を打つだけです — このカード自身の状態遷移は行いません。card には `ready` 状態も `ready` action も存在しません）。" +
			"card を進める・閉じる判断は行わないこと — それは人の accept、または khi の suggest 経由でのみ行われます" +
			"（card machine v2, docs/plans/suggestion-as-state-transition.md §3.2）。" +
			"更新の具体的な手順 (書き込み先・経路) はこの project 自身の CLAUDE.md やスキルに従うこと — " +
			"boid 側はここでは手順を指定しません。frontmatter やメタデータの直接編集が禁じられている project では、" +
			"それに従ってください。" +
			"整形の結果「やらない」と分かった場合は、このセッションでは何もせず運用者に破棄 (drop) の判断を委ねてください。")
	}
	return b.String()
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
