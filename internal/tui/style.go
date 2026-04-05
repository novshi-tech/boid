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
)
