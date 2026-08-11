package api

import (
	"encoding/json"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/web/templates/components"
)

// BuildTreeItems takes a flat task list and returns items in DFS tree order
// with depth and visual-parent info. Siblings appear in input order.
// Tasks whose ParentID is absent from the list are treated as roots.
// Cycles in ParentID references are detected via a visited set and skipped.
//
// projectNames maps project ID to display name; pass nil to skip name resolution.
func BuildTreeItems(tasks []*orchestrator.Task, projectNames map[string]string) []components.TreeItem {
	idSet := make(map[string]bool, len(tasks))
	for _, t := range tasks {
		idSet[t.ID] = true
	}

	children := make(map[string][]*orchestrator.Task)
	var roots []*orchestrator.Task
	for _, t := range tasks {
		if t.ParentID == "" || !idSet[t.ParentID] {
			roots = append(roots, t)
		} else {
			children[t.ParentID] = append(children[t.ParentID], t)
		}
	}

	result := make([]components.TreeItem, 0, len(tasks))
	visited := make(map[string]bool, len(tasks))

	var dfs func(t *orchestrator.Task, depth int, parentID string)
	dfs = func(t *orchestrator.Task, depth int, parentID string) {
		if visited[t.ID] {
			return
		}
		visited[t.ID] = true
		result = append(result, components.TreeItem{
			Task:        t,
			Depth:       depth,
			HasChildren: len(children[t.ID]) > 0,
			ParentID:    parentID,
			ProjectName: projectNames[t.ProjectID],
		})
		for _, child := range children[t.ID] {
			dfs(child, depth+1, t.ID)
		}
	}
	for _, root := range roots {
		dfs(root, 0, "")
	}

	return result
}

// BuildFlatItems returns tasks as a flat list (Depth=0, HasChildren=false, ParentID="").
// Used for the "closed" status view where tree structure is irrelevant.
func BuildFlatItems(tasks []*orchestrator.Task, projectNames map[string]string) []components.TreeItem {
	result := make([]components.TreeItem, 0, len(tasks))
	for _, t := range tasks {
		result = append(result, components.TreeItem{
			Task:        t,
			Depth:       0,
			HasChildren: false,
			ParentID:    "",
			ProjectName: projectNames[t.ProjectID],
		})
	}
	return result
}

// BuildQueueItems returns tasks as a flat list (like BuildFlatItems) with
// Urgency/Summary populated from each task's task_triage row (when present)
// — the queue_next view's 文脈同梱 requirement (docs/plans/
// cross-project-issue-triage.md 成功の定義: 「1 項目 = 1 判断」). Input order is
// preserved untouched: store.go's ListTasks("queue_next") branch already
// applies queue 節 rule 3's ordering in SQL, so this must not re-sort.
//
// triageByTaskID maps task id to its task_triage row; a missing entry (task
// has no sidecar row) or a Detail blob that fails to parse both leave
// Urgency/Summary at their zero value rather than erroring — a queue row
// with no summary is still useful (title + urgency alone can be enough to
// judge), and a summary-less/malformed row must never make the WHOLE queue
// view fail to render (rule 5, 隠さない, would be defeated by that).
func BuildQueueItems(tasks []*orchestrator.Task, projectNames map[string]string, triageByTaskID map[string]*orchestrator.TaskTriage) []components.TreeItem {
	result := make([]components.TreeItem, 0, len(tasks))
	for _, t := range tasks {
		item := components.TreeItem{
			Task:        t,
			Depth:       0,
			HasChildren: false,
			ParentID:    "",
			ProjectName: projectNames[t.ProjectID],
		}
		if tt, ok := triageByTaskID[t.ID]; ok && tt != nil {
			item.Urgency = tt.Urgency
			item.Summary = triageSummary(tt.Detail)
		}
		result = append(result, item)
	}
	return result
}

// triageSummary extracts the "summary" string field from a task_triage
// detail JSON blob (docs/plans/cross-project-issue-triage.md データモデル節).
// Returns "" on any parse failure or when the field is absent — never
// panics, never partially-fabricates a value.
func triageSummary(detail []byte) string {
	if len(detail) == 0 {
		return ""
	}
	var v struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(detail, &v); err != nil {
		return ""
	}
	return v.Summary
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
