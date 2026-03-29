package testutil

import "sync"

type MockTmux struct {
	mu       sync.Mutex
	Sessions map[string][]string // session -> windows
}

func NewMockTmux() *MockTmux {
	return &MockTmux{Sessions: make(map[string][]string)}
}

func (m *MockTmux) EnsureSession(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.Sessions[name]; !ok {
		m.Sessions[name] = []string{}
	}
	return nil
}

func (m *MockTmux) NewWindow(session, windowName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sessions[session] = append(m.Sessions[session], windowName)
	return nil
}

func (m *MockTmux) RunInWindow(session, windowName, command string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Sessions[session] = append(m.Sessions[session], windowName)
	return nil
}

func (m *MockTmux) SendKeys(session, window, keys string) error {
	return nil
}

func (m *MockTmux) KillWindow(session, window string) error {
	return nil
}

func (m *MockTmux) ListWindows(session string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Sessions[session], nil
}

func (m *MockTmux) HasSession(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.Sessions[name]
	return ok
}

func (m *MockTmux) SwitchClient(session, window string) error {
	return nil
}

func (m *MockTmux) Attach(session string) error {
	return nil
}
