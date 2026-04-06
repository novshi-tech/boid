package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// stubScreen is a minimal Screen implementation for testing.
type stubScreen struct {
	name     string
	received []tea.Msg
}

func (s *stubScreen) Init() tea.Cmd                        { return nil }
func (s *stubScreen) Update(msg tea.Msg) (Screen, tea.Cmd) { s.received = append(s.received, msg); return s, nil }
func (s *stubScreen) View(width, height int) string        { return s.name }
func (s *stubScreen) ShortHelp() string                    { return s.name + " help" }

// newTestAppWithScreens creates an App whose stack contains the given screens.
func newTestAppWithScreens(screens ...Screen) *App {
	shared := &SharedState{Panes: make(map[string]string)}
	return &App{
		shared: shared,
		stack:  append([]Screen{}, screens...),
		width:  80,
		height: 24,
	}
}

func TestStack_PushPopPush(t *testing.T) {
	s1 := &stubScreen{name: "s1"}
	s2 := &stubScreen{name: "s2"}
	s3 := &stubScreen{name: "s3"}
	app := newTestAppWithScreens(s1)

	// push s2
	app.Update(pushScreenMsg{screen: s2})
	if top := app.stack[len(app.stack)-1]; top != s2 {
		t.Fatalf("after push s2: top = %v, want s2", top)
	}
	if len(app.stack) != 2 {
		t.Fatalf("after push s2: depth = %d, want 2", len(app.stack))
	}

	// pop
	app.Update(popScreenMsg{})
	if top := app.stack[len(app.stack)-1]; top != s1 {
		t.Fatalf("after pop: top = %v, want s1", top)
	}
	if len(app.stack) != 1 {
		t.Fatalf("after pop: depth = %d, want 1", len(app.stack))
	}

	// push s3
	app.Update(pushScreenMsg{screen: s3})
	if top := app.stack[len(app.stack)-1]; top != s3 {
		t.Fatalf("after push s3: top = %v, want s3", top)
	}
	if len(app.stack) != 2 {
		t.Fatalf("after push s3: depth = %d, want 2", len(app.stack))
	}
}

func TestUpdate_PushScreenMsg(t *testing.T) {
	s1 := &stubScreen{name: "s1"}
	s2 := &stubScreen{name: "s2"}
	app := newTestAppWithScreens(s1)

	app.Update(pushScreenMsg{screen: s2})

	if len(app.stack) != 2 {
		t.Fatalf("depth = %d, want 2", len(app.stack))
	}
	if app.stack[0] != s1 || app.stack[1] != s2 {
		t.Fatal("stack order incorrect")
	}
}

func TestUpdate_PopScreenMsg_DepthOne(t *testing.T) {
	s1 := &stubScreen{name: "s1"}
	app := newTestAppWithScreens(s1)

	app.Update(popScreenMsg{})

	// should not pop the last screen
	if len(app.stack) != 1 {
		t.Fatalf("depth = %d, want 1 (should not pop last screen)", len(app.stack))
	}
}

func TestGlobalKey_Quit(t *testing.T) {
	keys := []string{"q", "ctrl+c"}
	for _, key := range keys {
		for depth := 1; depth <= 3; depth++ {
			screens := make([]Screen, depth)
			for i := range screens {
				screens[i] = &stubScreen{name: "s"}
			}
			app := newTestAppWithScreens(screens...)

			var msg tea.KeyMsg
			if key == "ctrl+c" {
				msg = tea.KeyMsg{Type: tea.KeyCtrlC}
			} else {
				msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
			}
			_, cmd := app.Update(msg)

			if cmd == nil {
				t.Errorf("key=%q depth=%d: expected Quit cmd, got nil", key, depth)
				continue
			}
			result := cmd()
			if _, ok := result.(tea.QuitMsg); !ok {
				t.Errorf("key=%q depth=%d: expected QuitMsg, got %T", key, depth, result)
			}
		}
	}
}

func TestEsc_DepthOne_DelegatesToScreen(t *testing.T) {
	s1 := &stubScreen{name: "s1"}
	app := newTestAppWithScreens(s1)

	msg := tea.KeyMsg{Type: tea.KeyEscape}
	app.Update(msg)

	// esc at depth 1 should be delegated to the screen
	if len(s1.received) == 0 {
		t.Fatal("esc at depth 1 should be delegated to screen, but screen received nothing")
	}
	if len(app.stack) != 1 {
		t.Fatalf("depth = %d, want 1 (should not pop at depth 1)", len(app.stack))
	}
}

func TestEsc_DepthTwo_Pops(t *testing.T) {
	s1 := &stubScreen{name: "s1"}
	s2 := &stubScreen{name: "s2"}
	app := newTestAppWithScreens(s1, s2)

	msg := tea.KeyMsg{Type: tea.KeyEscape}
	app.Update(msg)

	if len(app.stack) != 1 {
		t.Fatalf("depth = %d, want 1 (esc should pop at depth 2)", len(app.stack))
	}
	if app.stack[0] != s1 {
		t.Fatal("after pop, top should be s1")
	}
}

func TestBackspace_DepthOne_DelegatesToScreen(t *testing.T) {
	s1 := &stubScreen{name: "s1"}
	app := newTestAppWithScreens(s1)

	msg := tea.KeyMsg{Type: tea.KeyBackspace}
	app.Update(msg)

	if len(s1.received) == 0 {
		t.Fatal("backspace at depth 1 should be delegated to screen")
	}
	if len(app.stack) != 1 {
		t.Fatalf("depth = %d, want 1", len(app.stack))
	}
}

func TestBackspace_DepthTwo_Pops(t *testing.T) {
	s1 := &stubScreen{name: "s1"}
	s2 := &stubScreen{name: "s2"}
	app := newTestAppWithScreens(s1, s2)

	msg := tea.KeyMsg{Type: tea.KeyBackspace}
	app.Update(msg)

	if len(app.stack) != 1 {
		t.Fatalf("depth = %d, want 1", len(app.stack))
	}
}

func TestWindowSizeMsg_PropagatedToAllScreens(t *testing.T) {
	s1 := &stubScreen{name: "s1"}
	s2 := &stubScreen{name: "s2"}
	app := newTestAppWithScreens(s1, s2)

	sizeMsg := tea.WindowSizeMsg{Width: 120, Height: 40}
	app.Update(sizeMsg)

	if app.width != 120 || app.height != 40 {
		t.Errorf("app size: got %dx%d, want 120x40", app.width, app.height)
	}
	// Both screens should have received the WindowSizeMsg
	for _, s := range []*stubScreen{s1, s2} {
		found := false
		for _, msg := range s.received {
			if _, ok := msg.(tea.WindowSizeMsg); ok {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("screen %q did not receive WindowSizeMsg", s.name)
		}
	}
}

func TestOtherKeys_DelegatedToTopScreen(t *testing.T) {
	s1 := &stubScreen{name: "s1"}
	s2 := &stubScreen{name: "s2"}
	app := newTestAppWithScreens(s1, s2)

	msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")}
	app.Update(msg)

	// s2 (top) should receive the key, s1 should not
	if len(s2.received) == 0 {
		t.Fatal("top screen should receive delegated key")
	}
	if len(s1.received) != 0 {
		t.Fatal("non-top screen should not receive delegated key")
	}
}
