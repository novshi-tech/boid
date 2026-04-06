package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func newTestApp() *App {
	return &App{activeFilter: "running", panes: make(map[string]string)}
}

func TestFilterKeys(t *testing.T) {
	cases := []struct {
		key    string
		want   string
	}{
		{"1", "all"},
		{"2", "running"},
		{"3", "pending"},
		{"4", "completed"},
		{"5", "failed"},
	}
	for _, tc := range cases {
		app := newTestApp()
		app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(tc.key)})
		if app.activeFilter != tc.want {
			t.Errorf("key %q: want filter %q, got %q", tc.key, tc.want, app.activeFilter)
		}
	}
}

func TestFilterKeys_ResetsCursor(t *testing.T) {
	app := newTestApp()
	app.cursor = 5
	app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	if app.cursor != 0 {
		t.Errorf("expected cursor 0 after filter change, got %d", app.cursor)
	}
}

func TestTabCycle(t *testing.T) {
	app := newTestApp()
	// 初期値は "running" (index 1)
	expected := []string{"pending", "completed", "failed", "all", "running"}
	for _, want := range expected {
		app.Update(tea.KeyMsg{Type: tea.KeyTab})
		if app.activeFilter != want {
			t.Errorf("tab: want filter %q, got %q", want, app.activeFilter)
		}
	}
}
