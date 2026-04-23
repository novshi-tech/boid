package api

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/web/templates/components"
)

// BuildTreeItems takes a flat task list and returns items in DFS tree order
// with depth and visual-parent info. Siblings appear in input order.
// Tasks whose ParentID is absent from the list are treated as roots.
// Cycles in ParentID references are detected via a visited set and skipped.
func BuildTreeItems(tasks []*orchestrator.Task) []components.TreeItem {
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
func BuildFlatItems(tasks []*orchestrator.Task) []components.TreeItem {
	result := make([]components.TreeItem, 0, len(tasks))
	for _, t := range tasks {
		result = append(result, components.TreeItem{
			Task:        t,
			Depth:       0,
			HasChildren: false,
			ParentID:    "",
		})
	}
	return result
}
