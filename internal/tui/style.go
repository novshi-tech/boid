package tui

import "github.com/charmbracelet/lipgloss"

var (
	styleHeader  = lipgloss.NewStyle().Bold(true)
	styleCursor  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("12"))
	styleRunning = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styleDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleFooter  = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styleBadge   = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))
	styleWarn    = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styleError   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))

	stylePending   = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))  // 黄色
	styleCompleted = lipgloss.NewStyle().Foreground(lipgloss.Color("240")) // グレー
	styleFailed    = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))   // 赤

	styleFilterActive   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	styleFilterInactive = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	// Task status styles
	styleExecuting = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))  // 緑系
	styleVerifying = lipgloss.NewStyle().Foreground(lipgloss.Color("12"))  // 青系
	styleTaskDim   = lipgloss.NewStyle().Foreground(lipgloss.Color("240")) // dim
	styleAborted   = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))   // 赤系

	// Table component styles
	styleTableHeader   = styleDim
	styleTableCell     = lipgloss.NewStyle()
	styleTableSelected = lipgloss.NewStyle().Background(lipgloss.Color("237"))

	// ツリー表示: 親タスクの見出し用
	styleTreeParent = lipgloss.NewStyle().Bold(true).Underline(true)
)
