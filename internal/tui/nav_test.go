package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// mockScreen is a minimal Screen implementation for testing.
type mockScreen struct {
	name     string
	updated  bool
	lastMsg  tea.Msg
	initCmd  tea.Cmd
	helpText string
}

func newMockScreen(name string) *mockScreen {
	return &mockScreen{name: name, helpText: name + " help"}
}

func (s *mockScreen) Init() tea.Cmd { return s.initCmd }

func (s *mockScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	s.updated = true
	s.lastMsg = msg
	return s, nil
}

func (s *mockScreen) View(width, height int) string {
	return s.name
}

func (s *mockScreen) ShortHelp() string {
	return s.helpText
}

func TestStackPushPop(t *testing.T) {
	screenA := newMockScreen("A")
	screenB := newMockScreen("B")
	screenC := newMockScreen("C")

	app := NewApp(nil, false)
	app.stack = []Screen{screenA}
	app.width = 80
	app.height = 24

	// push B
	app.Update(pushScreenMsg{screen: screenB})
	if len(app.stack) != 2 {
		t.Fatalf("expected stack depth 2 after push, got %d", len(app.stack))
	}
	if app.stack[1] != screenB {
		t.Fatal("expected screenB at top after push")
	}

	// pop -> back to A
	app.Update(popScreenMsg{})
	if len(app.stack) != 1 {
		t.Fatalf("expected stack depth 1 after pop, got %d", len(app.stack))
	}
	if app.stack[0] != screenA {
		t.Fatal("expected screenA at top after pop")
	}

	// push C
	app.Update(pushScreenMsg{screen: screenC})
	if len(app.stack) != 2 {
		t.Fatalf("expected stack depth 2 after second push, got %d", len(app.stack))
	}
	if app.stack[1] != screenC {
		t.Fatal("expected screenC at top after second push")
	}
}

func TestPopAtDepthOneIsNoop(t *testing.T) {
	screenA := newMockScreen("A")
	app := NewApp(nil, false)
	app.stack = []Screen{screenA}

	app.Update(popScreenMsg{})
	if len(app.stack) != 1 {
		t.Fatalf("expected stack depth 1 (pop at depth 1 should be noop), got %d", len(app.stack))
	}
}

func TestQuitKeyAtAnyDepth(t *testing.T) {
	screenA := newMockScreen("A")
	screenB := newMockScreen("B")

	// depth 1
	app := NewApp(nil, false)
	app.stack = []Screen{screenA}
	app.width = 80
	app.height = 24
	_, cmd := app.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("expected quit cmd at depth 1")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatal("expected tea.QuitMsg at depth 1")
	}

	// depth 2
	app2 := NewApp(nil, false)
	app2.stack = []Screen{screenA, screenB}
	app2.width = 80
	app2.height = 24
	_, cmd2 := app2.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd2 == nil {
		t.Fatal("expected quit cmd at depth 2")
	}
	msg2 := cmd2()
	if _, ok := msg2.(tea.QuitMsg); !ok {
		t.Fatal("expected tea.QuitMsg at depth 2")
	}
}

func TestCtrlCQuitsAtAnyDepth(t *testing.T) {
	screenA := newMockScreen("A")
	app := NewApp(nil, false)
	app.stack = []Screen{screenA}
	app.width = 80
	app.height = 24

	_, cmd := app.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd == nil {
		t.Fatal("expected quit cmd on ctrl+c")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatal("expected tea.QuitMsg on ctrl+c")
	}
}

func TestEscAtDepthOneDelegatesToScreen(t *testing.T) {
	screenA := newMockScreen("A")
	app := NewApp(nil, false)
	app.stack = []Screen{screenA}
	app.width = 80
	app.height = 24

	app.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if len(app.stack) != 1 {
		t.Fatal("esc at depth 1 should not pop")
	}
	if !screenA.updated {
		t.Fatal("esc at depth 1 should delegate to screen")
	}
}

func TestEscAtDepthTwoPops(t *testing.T) {
	screenA := newMockScreen("A")
	screenB := newMockScreen("B")
	app := NewApp(nil, false)
	app.stack = []Screen{screenA, screenB}
	app.width = 80
	app.height = 24

	app.Update(tea.KeyMsg{Type: tea.KeyEscape})
	if len(app.stack) != 1 {
		t.Fatalf("expected stack depth 1 after esc at depth 2, got %d", len(app.stack))
	}
}

func TestBackspaceAtDepthTwoPops(t *testing.T) {
	screenA := newMockScreen("A")
	screenB := newMockScreen("B")
	app := NewApp(nil, false)
	app.stack = []Screen{screenA, screenB}
	app.width = 80
	app.height = 24

	app.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if len(app.stack) != 1 {
		t.Fatalf("expected stack depth 1 after backspace at depth 2, got %d", len(app.stack))
	}
}

func TestWindowSizePropagatesAllScreens(t *testing.T) {
	screenA := newMockScreen("A")
	screenB := newMockScreen("B")
	app := NewApp(nil, false)
	app.stack = []Screen{screenA, screenB}

	app.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !screenA.updated {
		t.Fatal("WindowSizeMsg should propagate to all screens, screenA not updated")
	}
	if !screenB.updated {
		t.Fatal("WindowSizeMsg should propagate to all screens, screenB not updated")
	}
	if app.width != 120 || app.height != 40 {
		t.Fatal("App width/height not updated")
	}
}

func TestOtherMsgDelegatesToTopScreen(t *testing.T) {
	screenA := newMockScreen("A")
	screenB := newMockScreen("B")
	app := NewApp(nil, false)
	app.stack = []Screen{screenA, screenB}
	app.width = 80
	app.height = 24

	app.Update(tickMsg{})
	if screenA.updated {
		t.Fatal("tickMsg should not go to screenA (not top)")
	}
	if !screenB.updated {
		t.Fatal("tickMsg should delegate to top screen (screenB)")
	}
}

func TestPushCallsInit(t *testing.T) {
	initCalled := false
	screenA := newMockScreen("A")
	screenB := newMockScreen("B")
	screenB.initCmd = func() tea.Msg {
		initCalled = true
		return nil
	}

	app := NewApp(nil, false)
	app.stack = []Screen{screenA}
	app.width = 80
	app.height = 24

	_, cmd := app.Update(pushScreenMsg{screen: screenB})
	if cmd == nil {
		t.Fatal("expected Init cmd from pushed screen")
	}
	cmd()
	if !initCalled {
		t.Fatal("push should call Init on new screen")
	}
}

func TestViewRendersTopScreen(t *testing.T) {
	screenA := newMockScreen("A")
	screenB := newMockScreen("B")
	app := NewApp(nil, false)
	app.stack = []Screen{screenA, screenB}
	app.width = 80
	app.height = 24

	view := app.View()
	if view == "" {
		t.Fatal("expected non-empty view")
	}
	// The body should contain screenB's view content
	if !strings.Contains(view, "B") {
		t.Fatal("view should render top screen (B)")
	}
}
