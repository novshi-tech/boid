package api

import (
	"github.com/novshi-tech/boid/web/templates"
)

// buildDepsTreeRows flattens the upstream and downstream dependency
// trees into two pre-order DFS lists — direct deps/dependents at
// depth 0, deeper chain members at depth 1, 2, … — so the template
// can render each as a "parent above, child indented below" tree that
// matches the Open-tab task list.
func buildDepsTreeRows(up, down []*TaskNode) (upstream, downstream []templates.DepsTreeRow) {
	upstream = flattenDepsTree(up, 0)
	downstream = flattenDepsTree(down, 0)
	return upstream, downstream
}

// flattenDepsTree walks a dependency tree pre-order so the node is
// emitted before its children — that gives "closer-to-self first,
// deeper members indented below".
func flattenDepsTree(nodes []*TaskNode, depth int) []templates.DepsTreeRow {
	var rows []templates.DepsTreeRow
	for _, node := range nodes {
		rows = append(rows, templates.DepsTreeRow{Task: node.Task, Depth: depth})
		rows = append(rows, flattenDepsTree(node.Children, depth+1)...)
	}
	return rows
}
