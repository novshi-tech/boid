package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestNewApp_HomeIsTaskListScreen(t *testing.T) {
	app := NewApp(nil, false)
	if len(app.stack) != 1 {
		t.Fatalf("expected 1 screen on stack, got %d", len(app.stack))
	}
	if _, ok := app.stack[0].(*TaskListScreen); !ok {
		t.Errorf("expected TaskListScreen as home screen, got %T", app.stack[0])
	}
}

// TestApp_QKey_DelegatedNotQuit verifies q is not intercepted by App.Update.
func TestApp_QKey_DelegatedNotQuit(t *testing.T) {
	s := &stubScreen{name: "s"}
	app := newTestAppWithScreens(s)

	_, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})

	// stubScreen returns nil, so App should return nil (not tea.Quit)
	if cmd != nil {
		result := cmd()
		if _, ok := result.(tea.QuitMsg); ok {
			t.Error("q key: App should not intercept and return tea.Quit (should delegate to screen)")
		}
	}
	// Top screen should have received the q key
	if len(s.received) == 0 {
		t.Error("q key: should be delegated to the top screen, but screen received nothing")
	}
}

// fixedViewScreen is a Screen whose View() always returns a preset string,
// used to control output length independently of width/height.
type fixedViewScreen struct {
	viewContent string
	helpText    string
}

func (s *fixedViewScreen) Init() tea.Cmd                        { return nil }
func (s *fixedViewScreen) Update(msg tea.Msg) (Screen, tea.Cmd) { return s, nil }
func (s *fixedViewScreen) View(width, height int) string        { return s.viewContent }
func (s *fixedViewScreen) ShortHelp() string                    { return s.helpText }

// TestApp_View_FooterFixedAtBottom verifies that App.View() always produces
// exactly m.height visual rows (footer anchored to bottom) regardless of how
// many rows the Screen's View() returns.
func TestApp_View_FooterFixedAtBottom(t *testing.T) {
	const termWidth = 80
	const termHeight = 24
	const bodyHeight = termHeight - 4 // header(2) + footer(2)

	cases := []struct {
		name        string
		viewContent string
	}{
		{"no newline (1 visual line)", "one line"},
		{"trailing newline (1 content line)", "one line\n"},
		{"multi-line no trailing newline", "line1\nline2\nline3"},
		{"multi-line with trailing newline", "line1\nline2\nline3\n"},
		{"empty string", ""},
		{"exactly bodyHeight lines with trailing newline", strings.Repeat("x\n", bodyHeight)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			screen := &fixedViewScreen{viewContent: tc.viewContent, helpText: "test help"}
			app := &App{
				shared: &SharedState{Panes: make(map[string]string)},
				stack:  []Screen{screen},
				width:  termWidth,
				height: termHeight,
			}

			output := app.View()

			// Total visual rows must equal terminal height.
			if got := lipgloss.Height(output); got != termHeight {
				t.Errorf("lipgloss.Height(View()) = %d, want %d\noutput:\n%s", got, termHeight, output)
			}

			// Footer must occupy the final two lines: separator then ShortHelp.
			lines := strings.Split(output, "\n")
			if len(lines) < 2 {
				t.Fatalf("output has only %d line(s), expected at least 2", len(lines))
			}
			footerSep := lines[len(lines)-2]
			footerHelp := lines[len(lines)-1]

			if !strings.Contains(footerSep, "─") {
				t.Errorf("second-to-last line should be the footer separator (─), got %q", footerSep)
			}
			if !strings.Contains(footerHelp, "test help") {
				t.Errorf("last line should contain ShortHelp() text, got %q", footerHelp)
			}
		})
	}
}

// TestApp_View_FooterFixedAtBottom_WithWindowSizeMsg verifies the same guarantee
// after the app receives a tea.WindowSizeMsg (the normal startup path).
func TestApp_View_FooterFixedAtBottom_WithWindowSizeMsg(t *testing.T) {
	const w, h = 100, 30
	screen := &fixedViewScreen{viewContent: "short", helpText: "help"}
	app := newTestAppWithScreens(screen)

	app.Update(tea.WindowSizeMsg{Width: w, Height: h})

	output := app.View()
	if got := lipgloss.Height(output); got != h {
		t.Errorf("after WindowSizeMsg: lipgloss.Height(View()) = %d, want %d", got, h)
	}

	lines := strings.Split(output, "\n")
	footerHelp := lines[len(lines)-1]
	if !strings.Contains(footerHelp, "help") {
		t.Errorf("last line should contain ShortHelp, got %q", footerHelp)
	}
}

// TestApp_CtrlC_ReturnsQuit verifies ctrl+c is still intercepted by App as emergency exit.
func TestApp_CtrlC_ReturnsQuit(t *testing.T) {
	s := &stubScreen{name: "s"}
	app := newTestAppWithScreens(s)

	_, cmd := app.Update(tea.KeyMsg{Type: tea.KeyCtrlC})

	if cmd == nil {
		t.Fatal("ctrl+c: expected non-nil cmd (tea.Quit)")
	}
	result := cmd()
	if _, ok := result.(tea.QuitMsg); !ok {
		t.Errorf("ctrl+c: expected QuitMsg, got %T", result)
	}
	// ctrl+c should NOT be delegated to screen (intercepted at app level)
	if len(s.received) != 0 {
		t.Error("ctrl+c: should be intercepted by App, not delegated to screen")
	}
}
