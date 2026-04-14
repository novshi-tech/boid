package tui

import (
	"fmt"
	"strings"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// depsTreeRow is one row in the flattened deps-tree display.
type depsTreeRow struct {
	task   *orchestrator.Task
	isSelf bool
	prefix string // visual prefix (indentation and/or connector)
}

// buildDepsTreeRows builds the flat list for the deps tree display:
// upstream rows (deepest ancestor first) → self → downstream rows.
func buildDepsTreeRows(detail *api.TaskDetailView) []depsTreeRow {
	if detail == nil {
		return nil
	}

	var rows []depsTreeRow

	// 1. Upstream: post-order DFS (children before parent = deeper ancestors first).
	maxUp := treeMaxDepth(detail.DependsOnTree, 0)
	upRows := flattenUpstream(detail.DependsOnTree, 0, maxUp)
	rows = append(rows, upRows...)

	// 2. Self
	if detail.Task != nil {
		rows = append(rows, depsTreeRow{task: detail.Task, isSelf: true})
	}

	// 3. Downstream: pre-order DFS with ├─/└─ connectors.
	downRows := flattenDownstream(detail.DependentsTree, "")
	rows = append(rows, downRows...)

	return rows
}

// treeMaxDepth returns the maximum depth of a TaskNode tree (root = depth 0).
func treeMaxDepth(nodes []*api.TaskNode, depth int) int {
	maxD := depth
	for _, n := range nodes {
		if d := treeMaxDepth(n.Children, depth+1); d > maxD {
			maxD = d
		}
	}
	return maxD
}

// flattenUpstream collects upstream rows in post-order DFS (children before parent),
// so deeper ancestors appear first (higher in the display).
// depth = depth within DependsOnTree (0 = direct dep of self, 1 = grandparent, ...).
// maxDepth = maximum depth in the whole DependsOnTree.
// Visual indent = (maxDepth - depth) * 2 spaces, so deepest nodes have 0 indent.
func flattenUpstream(nodes []*api.TaskNode, depth, maxDepth int) []depsTreeRow {
	var rows []depsTreeRow
	for _, node := range nodes {
		// Children first (deeper ancestors appear higher in display).
		childRows := flattenUpstream(node.Children, depth+1, maxDepth)
		rows = append(rows, childRows...)
		// Then the node itself.
		indent := strings.Repeat("  ", maxDepth-depth)
		rows = append(rows, depsTreeRow{task: node.Task, prefix: indent})
	}
	return rows
}

// flattenDownstream collects downstream rows in pre-order DFS with ├─/└─ connectors.
// linePrefix is the prefix accumulated from parent levels.
func flattenDownstream(nodes []*api.TaskNode, linePrefix string) []depsTreeRow {
	var rows []depsTreeRow
	for i, node := range nodes {
		isLast := i == len(nodes)-1
		var nodeConnector, childContinuation string
		if isLast {
			nodeConnector = "└─ "
			childContinuation = "   "
		} else {
			nodeConnector = "├─ "
			childContinuation = "│  "
		}
		rows = append(rows, depsTreeRow{task: node.Task, prefix: linePrefix + nodeConnector})
		childRows := flattenDownstream(node.Children, linePrefix+childContinuation)
		rows = append(rows, childRows...)
	}
	return rows
}

// depSelectableItems returns the list of tasks that can be navigated to
// from the Deps tab, in the order they appear in the tree display
// (upstream post-order DFS, then downstream pre-order DFS).
func depSelectableItems(detail *api.TaskDetailView) []*orchestrator.Task {
	if detail == nil {
		return nil
	}
	rows := buildDepsTreeRows(detail)
	var items []*orchestrator.Task
	for _, r := range rows {
		if !r.isSelf {
			items = append(items, r.task)
		}
	}
	return items
}

// renderDeps renders the Deps tab as a single tree with self in the middle:
// upstream (ancestors) above, self, downstream (descendants) below.
// cursor is the index into depSelectableItems(detail).
func renderDeps(detail *api.TaskDetailView, width, height, cursor int) string {
	if detail == nil {
		return styleDim.Render("  (loading...)") + "\n"
	}

	rows := buildDepsTreeRows(detail)

	// Check if there are any non-self rows.
	hasDeps := false
	for _, r := range rows {
		if !r.isSelf {
			hasDeps = true
			break
		}
	}
	if !hasDeps {
		return styleDim.Render("  no dependencies") + "\n"
	}

	// Build selectable-to-row mapping (selectable index → visual row index).
	selectableToRow := make([]int, 0, len(rows))
	for i, r := range rows {
		if !r.isSelf {
			selectableToRow = append(selectableToRow, i)
		}
	}

	// Compute scroll offset to keep cursor row in view.
	visualCursor := -1
	if cursor >= 0 && cursor < len(selectableToRow) {
		visualCursor = selectableToRow[cursor]
	}
	scroll := 0
	if visualCursor >= height {
		scroll = visualCursor - height + 1
	}
	if maxScroll := max(len(rows)-height, 0); scroll > maxScroll {
		scroll = maxScroll
	}

	// Count selectable rows before the scroll window (to compute selectableIdx correctly).
	selectableBase := 0
	for i := 0; i < scroll; i++ {
		if !rows[i].isSelf {
			selectableBase++
		}
	}

	var sb strings.Builder
	selectableIdx := selectableBase
	end := min(scroll+height, len(rows))
	for i := scroll; i < end; i++ {
		row := rows[i]
		if row.isSelf {
			sb.WriteString(renderSelfDepsRow(row.task, width))
		} else {
			selected := selectableIdx == cursor
			sb.WriteString(renderDepsTreeItem(row.task, row.prefix, selected, width))
			selectableIdx++
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

// renderSelfDepsRow renders the "this task" row (always shown with selected-row background).
func renderSelfDepsRow(task *orchestrator.Task, _ int) string {
	if task == nil {
		return styleTableSelected.Render("  (this task)")
	}
	_, statusText := taskStatusDisplay(task.Status)
	idStr := styleDim.Render(shortID(task.ID))
	title := truncate(task.Title, 40)
	line := fmt.Sprintf("  %-8s  %-12s  %s  (this task)", idStr, statusText, title)
	return styleTableSelected.Render(reinforceSelectedBg(line))
}

// renderDepsTreeItem renders one selectable row in the deps tree.
func renderDepsTreeItem(task *orchestrator.Task, prefix string, selected bool, _ int) string {
	_, statusText := taskStatusDisplay(task.Status)
	idStr := styleDim.Render(shortID(task.ID))
	title := truncate(task.Title, 40)
	line := fmt.Sprintf("%s%-8s  %-12s  %s", prefix, idStr, statusText, title)
	if selected {
		return styleTableSelected.Render(reinforceSelectedBg(line))
	}
	return line
}
