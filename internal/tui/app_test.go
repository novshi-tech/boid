package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// newTestActiveJobsApp creates an App with an ActiveJobsScreen for testing.
func newTestActiveJobsApp() (*App, *ActiveJobsScreen) {
	shared := &SharedState{Panes: make(map[string]string)}
	screen := &ActiveJobsScreen{shared: shared, activeFilter: "running"}
	app := &App{
		shared: shared,
		stack:  []Screen{screen},
		width:  80,
		height: 24,
	}
	return app, screen
}

func TestFilterKeys(t *testing.T) {
	cases := []struct {
		key  string
		want string
	}{
		{"1", "all"},
		{"2", "running"},
		{"3", "pending"},
		{"4", "completed"},
		{"5", "failed"},
	}
	for _, tc := range cases {
		app, screen := newTestActiveJobsApp()
		app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
		if screen.activeFilter != tc.want {
			t.Errorf("key %q: want filter %q, got %q", tc.key, tc.want, screen.activeFilter)
		}
	}
}

func TestFilterKeys_ResetsCursor(t *testing.T) {
	app, screen := newTestActiveJobsApp()
	screen.cursor = 5
	app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	if screen.cursor != 0 {
		t.Errorf("expected cursor 0 after filter change, got %d", screen.cursor)
	}
}

func TestTabCycle(t *testing.T) {
	app, screen := newTestActiveJobsApp()
	// initial: "running" (index 1)
	expected := []string{"pending", "completed", "failed", "all", "running"}
	for _, want := range expected {
		app.Update(tea.KeyMsg{Type: tea.KeyTab})
		if screen.activeFilter != want {
			t.Errorf("tab: want filter %q, got %q", want, screen.activeFilter)
		}
	}
}

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
