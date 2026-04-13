package tui

import (
	"fmt"
	"strings"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// depSelectableItems returns the list of tasks that can be navigated to
// from the Deps tab. The order is: DependsOnResolved first, then Dependents.
func depSelectableItems(detail *api.TaskDetailView) []*orchestrator.Task {
	if detail == nil {
		return nil
	}
	var items []*orchestrator.Task
	items = append(items, detail.DependsOnResolved...)
	items = append(items, detail.Dependents...)
	return items
}

// renderDeps renders the Deps tab content as a tree with cursor support.
// cursor is the index into depSelectableItems(detail).
func renderDeps(detail *api.TaskDetailView, width, height, cursor int) string {
	if detail == nil {
		return styleDim.Render("  (loading...)") + "\n"
	}

	hasDeps := len(detail.DependsOnResolved) > 0 ||
		len(detail.Dependents) > 0 ||
		(detail.Task != nil && len(detail.Task.DependsOn) > 0)
	if !hasDeps {
		return styleDim.Render("  no dependencies") + "\n"
	}

	var sb strings.Builder
	selectableIdx := 0
	lines := 0

	// Build a set of resolved task IDs for unresolved detection.
	resolvedIDs := make(map[string]struct{}, len(detail.DependsOnResolved))
	for _, t := range detail.DependsOnResolved {
		resolvedIDs[t.ID] = struct{}{}
	}

	// ─── Depends on ───────────────────────────────────────────
	sb.WriteString(renderSectionHeader("Depends on", width))
	sb.WriteByte('\n')
	lines++

	dependsOnIDs := []string{}
	if detail.Task != nil {
		dependsOnIDs = detail.Task.DependsOn
	}

	if len(detail.DependsOnResolved) == 0 && len(dependsOnIDs) == 0 {
		sb.WriteString(styleDim.Render("  (none)"))
		sb.WriteByte('\n')
		lines++
	} else {
		// Resolved depends_on items (cursor-selectable).
		for _, t := range detail.DependsOnResolved {
			if lines >= height {
				break
			}
			selected := selectableIdx == cursor
			sb.WriteString(renderDepsItem(t, selected, width))
			sb.WriteByte('\n')
			lines++
			selectableIdx++
		}
		// Unresolved depends_on entries (not cursor-selectable).
		for _, id := range dependsOnIDs {
			if lines >= height {
				break
			}
			if _, ok := resolvedIDs[id]; ok {
				continue
			}
			sb.WriteString(styleDim.Render(fmt.Sprintf("  (unresolved: %s)", shortID(id))))
			sb.WriteByte('\n')
			lines++
		}
	}

	if lines >= height {
		return sb.String()
	}

	// ─── Dependents ───────────────────────────────────────────
	sb.WriteString(renderSectionHeader("Dependents", width))
	sb.WriteByte('\n')
	lines++

	if len(detail.Dependents) == 0 {
		sb.WriteString(styleDim.Render("  (none)"))
		sb.WriteByte('\n')
	} else {
		for _, t := range detail.Dependents {
			if lines >= height {
				break
			}
			selected := selectableIdx == cursor
			sb.WriteString(renderDepsItem(t, selected, width))
			sb.WriteByte('\n')
			lines++
			selectableIdx++
		}
	}

	return sb.String()
}

// renderDepsItem renders a single dependency line with cursor indicator.
func renderDepsItem(task *orchestrator.Task, selected bool, _ int) string {
	cursorStr := "  "
	if selected {
		cursorStr = styleCursor.Render("▸ ")
	}
	_, statusText := taskStatusDisplay(task.Status)
	idStr := styleDim.Render(shortID(task.ID))
	title := truncate(task.Title, 40)
	return fmt.Sprintf("%s%-8s  %-12s  %s", cursorStr, idStr, statusText, title)
}
