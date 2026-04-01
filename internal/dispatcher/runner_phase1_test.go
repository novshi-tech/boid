package dispatcher_test

import (
	"context"
	"os"
	"sort"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

type statefulTmux struct {
	mu       sync.Mutex
	sessions map[string]map[string]string
}

func newStatefulTmux() *statefulTmux {
	return &statefulTmux{sessions: make(map[string]map[string]string)}
}

func (m *statefulTmux) EnsureSession(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[name]; !ok {
		m.sessions[name] = make(map[string]string)
	}
	return nil
}

func (m *statefulTmux) NewWindow(session, windowName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[session]; !ok {
		m.sessions[session] = make(map[string]string)
	}
	m.sessions[session][windowName] = ""
	return nil
}

func (m *statefulTmux) RunInWindow(session, windowName, command string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[session]; !ok {
		m.sessions[session] = make(map[string]string)
	}
	m.sessions[session][windowName] = command
	return nil
}

func (m *statefulTmux) SendKeys(session, window, keys string) error { return nil }

func (m *statefulTmux) KillWindow(session, window string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[session]; ok {
		delete(m.sessions[session], window)
	}
	return nil
}

func (m *statefulTmux) ListWindows(session string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	windows := make([]string, 0, len(m.sessions[session]))
	for name := range m.sessions[session] {
		windows = append(windows, name)
	}
	sort.Strings(windows)
	return windows, nil
}

func (m *statefulTmux) HasSession(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.sessions[name]
	return ok
}

func (m *statefulTmux) Attach(session string) error               { return nil }
func (m *statefulTmux) SwitchClient(session, window string) error { return nil }

func cleanupSandboxScripts(t *testing.T, jobIDs ...string) {
	t.Helper()
	for _, jobID := range jobIDs {
		for _, suffix := range []string{"-inner.sh", "-setup.sh", "-outer.sh"} {
			_ = os.Remove("/tmp/boid-" + jobID + suffix)
		}
	}
}

func TestRunnerDispatch_SameTaskJobsNeedDistinctTmuxWindows(t *testing.T) {
	db := testutil.NewTestDB(t)
	projectDir := t.TempDir()

	if err := orchestrator.CreateProject(db.Conn, &orchestrator.Project{
		ID:      "proj-1",
		WorkDir: projectDir,
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	if err := orchestrator.CreateTask(db.Conn, &orchestrator.Task{
		ID:        "task-12345678-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID: "proj-1",
		Title:     "parallel hooks",
		Behavior:  "dev",
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}

	tmux := newStatefulTmux()
	runner := &dispatcher.Runner{
		DB:          db.Conn,
		Tmux:        tmux,
		TmuxSession: "boid",
		Sandbox: &fakeSandboxPreparer{
			outerPaths: []string{"/tmp/boid-hook-a.sh", "/tmp/boid-hook-b.sh"},
		},
	}

	planA := &dispatcher.DispatchPlan{
		TaskID:      "task-12345678-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID:   "proj-1",
		HandlerID:   "hook-a",
		Role:        "hook",
		ProjectDir:  projectDir,
		HomeDir:     projectDir,
		HookScript:  "hook-a.sh",
		BoidBinary:  "/bin/true",
		PayloadJSON: `{}`,
	}
	planB := &dispatcher.DispatchPlan{
		TaskID:      "task-12345678-abcd-efgh-ijkl-mnopqrstuvwx",
		ProjectID:   "proj-1",
		HandlerID:   "hook-b",
		Role:        "hook",
		ProjectDir:  projectDir,
		HomeDir:     projectDir,
		HookScript:  "hook-b.sh",
		BoidBinary:  "/bin/true",
		PayloadJSON: `{}`,
	}

	_, err := runner.Dispatch(context.Background(), planA)
	if err != nil {
		t.Fatalf("dispatch hook-a: %v", err)
	}
	_, err = runner.Dispatch(context.Background(), planB)
	if err != nil {
		t.Fatalf("dispatch hook-b: %v", err)
	}

	windows, err := tmux.ListWindows("boid")
	if err != nil {
		t.Fatalf("list windows: %v", err)
	}

	if len(windows) != 2 {
		t.Fatalf("same-task jobs should have distinct tmux windows; got %d windows: %v", len(windows), windows)
	}
}
