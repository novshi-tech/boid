package api

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/web/templates"
)

// buildDepsTreeRows flattens the upstream/downstream dependency trees into a
// single ordered list matching the TUI layout: deepest ancestors first (post-
// order DFS) → self → downstream dependents (pre-order DFS). Connectors
// ("├─ " / "└─ ") mirror the TUI's style so the tree is legible in plain HTML.
func buildDepsTreeRows(self *orchestrator.Task, up, down []*TaskNode) []templates.DepsTreeRow {
	var rows []templates.DepsTreeRow
	rows = append(rows, flattenUpstreamDeps(up, "")...)
	if self != nil {
		rows = append(rows, templates.DepsTreeRow{Task: self, IsSelf: true})
	}
	rows = append(rows, flattenDownstreamDeps(down, "")...)
	return rows
}

// flattenUpstreamDeps walks the upstream tree post-order so deeper ancestors
// appear first. Connector ("├─ ", "└─ ") is picked from sibling position and
// mirrors the downstream logic for visual consistency.
func flattenUpstreamDeps(nodes []*TaskNode, linePrefix string) []templates.DepsTreeRow {
	var rows []templates.DepsTreeRow
	for i, node := range nodes {
		isLast := i == len(nodes)-1
		var nodeConnector, childCont string
		if isLast {
			nodeConnector = "└─ "
			childCont = "   "
		} else {
			nodeConnector = "├─ "
			childCont = "│  "
		}
		rows = append(rows, flattenUpstreamDeps(node.Children, linePrefix+childCont)...)
		rows = append(rows, templates.DepsTreeRow{Task: node.Task, Prefix: linePrefix + nodeConnector})
	}
	return rows
}

// flattenDownstreamDeps walks the downstream tree pre-order (natural read order).
func flattenDownstreamDeps(nodes []*TaskNode, linePrefix string) []templates.DepsTreeRow {
	var rows []templates.DepsTreeRow
	for i, node := range nodes {
		isLast := i == len(nodes)-1
		var nodeConnector, childCont string
		if isLast {
			nodeConnector = "└─ "
			childCont = "   "
		} else {
			nodeConnector = "├─ "
			childCont = "│  "
		}
		rows = append(rows, templates.DepsTreeRow{Task: node.Task, Prefix: linePrefix + nodeConnector})
		rows = append(rows, flattenDownstreamDeps(node.Children, linePrefix+childCont)...)
	}
	return rows
}
