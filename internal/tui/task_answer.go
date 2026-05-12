package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- focus states ---

type answerFocus int

const (
	answerFocusEditor answerFocus = iota
	answerFocusSubmit
	answerFocusCancel
	answerFocusCount
)

// answerResultMsg is sent when AnswerTask API call completes.
type answerResultMsg struct{ err error }

// TaskAnswerScreen lets the user answer an agent question while the task is awaiting.
// In read-only mode (answer already submitted) it shows the question and the pending answer.
type TaskAnswerScreen struct {
	client     *client.Client
	taskID     string
	taskTitle  string
	question   string
	questionID string
	readOnly   bool // true when pending_answer is already set

	editor    TextAreaModel
	submitBtn ButtonModel
	cancelBtn ButtonModel

	focusIndex answerFocus
	errMsg     string
	submitting bool
}

// NewTaskAnswerScreen builds the answer screen from an AwaitingPayload.
// When pendingAnswer is non-empty the screen opens in read-only mode.
func NewTaskAnswerScreen(c *client.Client, task *orchestrator.Task, ap orchestrator.AwaitingPayload) *TaskAnswerScreen {
	readOnly := ap.PendingAnswer != ""

	ta := NewTextArea()
	ta.SetLabel("answer")
	ta.SetHeight(8)
	ta.SetPlaceholder("Enter your answer…")
	if readOnly {
		ta.SetValue(ap.PendingAnswer)
	}

	s := &TaskAnswerScreen{
		client:     c,
		taskID:     task.ID,
		taskTitle:  task.Title,
		question:   ap.Question,
		questionID: ap.QuestionID,
		readOnly:   readOnly,
		editor:     ta,
		submitBtn:  NewButton("Submit"),
		cancelBtn:  NewButton("Cancel"),
		focusIndex: answerFocusEditor,
	}
	return s
}

func (s *TaskAnswerScreen) Init() tea.Cmd {
	if s.readOnly {
		return nil
	}
	return s.editor.Focus()
}

func (s *TaskAnswerScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case answerResultMsg:
		s.submitting = false
		if msg.err != nil {
			s.errMsg = "submit failed: " + msg.err.Error()
			return s, nil
		}
		return s, func() tea.Msg { return popScreenMsg{} }

	case ButtonPressedMsg:
		switch msg.Label {
		case "Submit":
			return s, s.submit()
		case "Cancel":
			return s, func() tea.Msg { return popScreenMsg{} }
		}

	case tea.KeyMsg:
		return s, s.handleKey(msg)
	}
	return s, nil
}

func (s *TaskAnswerScreen) handleKey(msg tea.KeyMsg) tea.Cmd {
	switch msg.String() {
	case "esc":
		return func() tea.Msg { return popScreenMsg{} }
	case "tab":
		if !s.readOnly {
			return s.shiftFocus(1)
		}
	case "shift+tab":
		if !s.readOnly {
			return s.shiftFocus(-1)
		}
	}

	if s.readOnly {
		return nil
	}

	switch s.focusIndex {
	case answerFocusEditor:
		var cmd tea.Cmd
		s.editor, cmd = s.editor.Update(msg)
		return cmd
	case answerFocusSubmit:
		var cmd tea.Cmd
		s.submitBtn, cmd = s.submitBtn.Update(msg)
		return cmd
	case answerFocusCancel:
		var cmd tea.Cmd
		s.cancelBtn, cmd = s.cancelBtn.Update(msg)
		return cmd
	}
	return nil
}

func (s *TaskAnswerScreen) shiftFocus(delta int) tea.Cmd {
	s.blurCurrent()
	next := int(s.focusIndex) + delta
	if next < 0 {
		next += int(answerFocusCount)
	}
	s.focusIndex = answerFocus(next % int(answerFocusCount))
	return s.focusCurrent()
}

func (s *TaskAnswerScreen) blurCurrent() {
	switch s.focusIndex {
	case answerFocusEditor:
		s.editor.Blur()
	case answerFocusSubmit:
		s.submitBtn.Blur()
	case answerFocusCancel:
		s.cancelBtn.Blur()
	}
}

func (s *TaskAnswerScreen) focusCurrent() tea.Cmd {
	switch s.focusIndex {
	case answerFocusEditor:
		return s.editor.Focus()
	case answerFocusSubmit:
		return s.submitBtn.Focus()
	case answerFocusCancel:
		return s.cancelBtn.Focus()
	}
	return nil
}

func (s *TaskAnswerScreen) submit() tea.Cmd {
	s.errMsg = ""
	answer := strings.TrimSpace(s.editor.Value())
	if answer == "" {
		s.errMsg = "answer cannot be empty"
		return nil
	}
	s.submitting = true
	taskID := s.taskID
	questionID := s.questionID
	return func() tea.Msg {
		err := s.client.AnswerTask(taskID, questionID, answer)
		return answerResultMsg{err: err}
	}
}

// styleQuestionBox is the lipgloss border box used for the question text.
var styleQuestionBox = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(lipgloss.Color("12")).
	Padding(0, 1)

func (s *TaskAnswerScreen) View(width, height int) string {
	var sb strings.Builder

	sb.WriteString(styleTitle.Render("Question from agent"))
	sb.WriteByte('\n')
	sb.WriteString(strings.Repeat("─", width))
	sb.WriteByte('\n')

	sb.WriteString(styleDim.Render("Task: " + s.taskTitle))
	sb.WriteByte('\n')
	sb.WriteByte('\n')

	// Question text in a border box
	innerWidth := max(width-4, 10) // account for border + padding
	question := s.question
	if question == "" {
		question = "(no question text)"
	}
	wrapped := wrapText(question, innerWidth)
	sb.WriteString(styleQuestionBox.Width(innerWidth).Render(wrapped))
	sb.WriteByte('\n')
	sb.WriteByte('\n')

	if s.readOnly {
		sb.WriteString(styleDim.Render("  Answer already submitted (waiting for agent):"))
		sb.WriteByte('\n')
		sb.WriteString(styleDim.Render("  " + strings.ReplaceAll(s.editor.Value(), "\n", "\n  ")))
		sb.WriteByte('\n')
	} else {
		s.editor.SetWidth(width)
		sb.WriteString(s.editor.View())
		sb.WriteString("\n  " + s.submitBtn.View() + "    " + s.cancelBtn.View() + "\n")
	}

	if s.errMsg != "" {
		sb.WriteString(styleError.Render("  ! " + s.errMsg))
		sb.WriteByte('\n')
	}

	_ = height
	return sb.String()
}

func (s *TaskAnswerScreen) ShortHelp() string {
	if s.readOnly {
		return "esc: back"
	}
	return "tab: next  shift+tab: prev  esc: cancel"
}

// wrapText wraps text to at most width runes per line, preserving existing newlines.
func wrapText(text string, width int) string {
	if width <= 0 {
		return text
	}
	var out strings.Builder
	for i, line := range strings.Split(text, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		runes := []rune(line)
		for len(runes) > width {
			// find last space within width
			cut := width
			for j := width - 1; j >= 0; j-- {
				if runes[j] == ' ' {
					cut = j + 1
					break
				}
			}
			out.WriteString(string(runes[:cut]))
			out.WriteByte('\n')
			runes = runes[cut:]
		}
		out.WriteString(string(runes))
	}
	return out.String()
}
