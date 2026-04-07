package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func newTestApp() *App {
	shared := &SharedState{Width: 120, Height: 40}
	return &App{
		shared:       shared,
		panes:        make(map[string]string),
		activeFilter: "running",
	}
}

func TestAppQuit(t *testing.T) {
	app := newTestApp()
	home := NewTaskListScreen(app.shared)
	app.screens = []Screen{home}

	_, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Errorf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestAppWindowResize(t *testing.T) {
	app := newTestApp()
	home := NewTaskListScreen(app.shared)
	app.screens = []Screen{home}

	app.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	if app.shared.Width != 200 || app.shared.Height != 50 {
		t.Errorf("expected shared state updated, got %dx%d", app.shared.Width, app.shared.Height)
	}
}

func TestAppInitPushesHomeScreen(t *testing.T) {
	app := newTestApp()
	app.Init()
	if len(app.screens) != 1 {
		t.Fatalf("expected 1 screen on stack, got %d", len(app.screens))
	}
	if _, ok := app.screens[0].(*TaskListScreen); !ok {
		t.Errorf("expected TaskListScreen, got %T", app.screens[0])
	}
}

func TestAppDelegatesKeyToScreen(t *testing.T) {
	app := newTestApp()
	home := NewTaskListScreen(app.shared)
	app.screens = []Screen{home}

	// Tab should change the screen's filter, not be handled by App
	app.Update(tea.KeyMsg{Type: tea.KeyTab})
	screen := app.screens[0].(*TaskListScreen)
	if screen.statusFilter == "active" {
		t.Error("expected tab to change filter on the screen, but it's still 'active'")
	}
}
