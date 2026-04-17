package tui

import (
	"sort"
	"strings"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"gopkg.in/yaml.v3"
)

// instructionRoleItem represents one role entry under task.Instructions.
type instructionRoleItem struct {
	role        string
	instruction orchestrator.Instruction
}

// extractInstructionRoles returns task.Instructions entries sorted by role name.
func extractInstructionRoles(m map[string]orchestrator.Instruction) []instructionRoleItem {
	if len(m) == 0 {
		return nil
	}
	roles := make([]string, 0, len(m))
	for r := range m {
		roles = append(roles, r)
	}
	sort.Strings(roles)
	out := make([]instructionRoleItem, 0, len(roles))
	for _, r := range roles {
		out = append(out, instructionRoleItem{role: r, instruction: m[r]})
	}
	return out
}

// instructionToYAML renders a single Instruction as YAML for preview/edit.
func instructionToYAML(inst orchestrator.Instruction) (string, error) {
	out, err := yaml.Marshal(inst)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// renderInstructions renders the Instructions tab: role list on top, YAML
// preview of the selected role's instruction below. Mirrors the layout of
// renderPayload so users get a consistent experience across tabs.
func renderInstructions(detail *api.TaskDetailView, cursor, previewScroll, width, height int) string {
	if detail == nil || detail.Task == nil {
		return styleDim.Render("  (no task data)") + "\n"
	}

	roles := extractInstructionRoles(detail.Task.Instructions)
	if len(roles) == 0 {
		return styleDim.Render("  (no instructions)") + "\n"
	}

	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Roles:"))
	sb.WriteByte('\n')

	maxListLines := min(max(height/3, 3), len(roles))

	for i, r := range roles {
		if i >= maxListLines {
			break
		}
		summary := r.role + "  " + string(r.instruction.Type) + "  " + r.instruction.Consumer
		if r.instruction.Model != "" {
			summary += "  " + r.instruction.Model
		}
		var line string
		if i == cursor {
			line = styleCursor.Render("  ▸ ") + styleTitle.Render(summary) + styleDim.Render("   (edit: e)")
		} else {
			line = "    " + styleDim.Render(summary)
		}
		sb.WriteString(line)
		sb.WriteByte('\n')
	}

	var selected *instructionRoleItem
	if cursor >= 0 && cursor < len(roles) {
		selected = &roles[cursor]
	}

	previewLabel := ""
	if selected != nil {
		previewLabel = "Preview (" + selected.role + ") "
	}
	fillLen := max(width-4-len([]rune(previewLabel))-4, 0)
	sep := styleDim.Render("─── " + previewLabel + strings.Repeat("─", fillLen))
	sb.WriteString(sep)
	sb.WriteByte('\n')

	previewHeight := max(height-1-maxListLines-1, 2)

	if selected == nil {
		sb.WriteString(styleDim.Render("  (select a role with j/k)"))
		sb.WriteByte('\n')
		return sb.String()
	}

	yamlStr, err := instructionToYAML(selected.instruction)
	if err != nil {
		sb.WriteString(styleError.Render("  error: " + err.Error()))
		sb.WriteByte('\n')
		return sb.String()
	}

	lines := strings.Split(strings.TrimRight(yamlStr, "\n"), "\n")
	start := min(previewScroll, max(len(lines)-1, 0))
	end := min(start+previewHeight, len(lines))

	for _, line := range lines[start:end] {
		sb.WriteString("  " + line)
		sb.WriteByte('\n')
	}

	return sb.String()
}
