package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
