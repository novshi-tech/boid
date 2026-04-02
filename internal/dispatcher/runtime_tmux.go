package dispatcher

import (
	"context"
	"fmt"
	"sync"

	dtmux "github.com/novshi-tech/boid/internal/dispatcher/tmux"
)

type TmuxRuntime struct {
	Tmux    dtmux.TmuxManager
	Session string

	mu      sync.Mutex
	windows map[string]string
}

func (r *TmuxRuntime) session() string {
	if r.Session != "" {
		return r.Session
	}
	return "boid"
}

func (r *TmuxRuntime) Start(_ context.Context, spec RuntimeStartSpec) (*RuntimeHandle, error) {
	if r.Tmux == nil {
		return nil, fmt.Errorf("tmux manager is required")
	}
	if spec.Command == "" {
		return nil, fmt.Errorf("runtime command is required")
	}

	session := r.session()
	windowName := tmuxWindowName(spec)
	if err := r.Tmux.EnsureSession(session); err != nil {
		return nil, fmt.Errorf("ensure session: %w", err)
	}
	_ = r.Tmux.KillWindow(session, windowName)
	if err := r.Tmux.RunInWindow(session, windowName, spec.Command); err != nil {
		return nil, fmt.Errorf("run in window: %w", err)
	}

	runtimeID := fmt.Sprintf("tmux:%s:%s", session, windowName)
	r.track(runtimeID, windowName)
	return &RuntimeHandle{
		ID:          runtimeID,
		Interactive: spec.Interactive,
		TTY:         spec.TTY,
	}, nil
}

func (r *TmuxRuntime) Attach(_ context.Context, _ string, _ RuntimeAttachRequest) error {
	return ErrRuntimeUnsupported
}

func (r *TmuxRuntime) Resize(_ context.Context, _ string, _ TerminalSize) error {
	return ErrRuntimeUnsupported
}

func (r *TmuxRuntime) Wait(_ context.Context, _ string) (RuntimeExit, error) {
	return RuntimeExit{}, ErrRuntimeUnsupported
}

func (r *TmuxRuntime) Stop(_ context.Context, runtimeID string) error {
	if r.Tmux == nil {
		return fmt.Errorf("tmux manager is required")
	}

	windowName, ok := r.take(runtimeID)
	if !ok {
		return nil
	}
	return r.Tmux.KillWindow(r.session(), windowName)
}

func (r *TmuxRuntime) SupportsAttach(_ string) bool {
	return false
}

func (r *TmuxRuntime) track(runtimeID, windowName string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.windows == nil {
		r.windows = make(map[string]string)
	}
	r.windows[runtimeID] = windowName
}

func (r *TmuxRuntime) take(runtimeID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	windowName, ok := r.windows[runtimeID]
	if ok {
		delete(r.windows, runtimeID)
	}
	return windowName, ok
}

func tmuxWindowName(spec RuntimeStartSpec) string {
	if spec.Role == "hook" || spec.Role == "gate" {
		return fmt.Sprintf("job-%s-%s", shortID(spec.TaskID), shortID(spec.JobID))
	}
	return fmt.Sprintf("hook-%s-%s", shortID(spec.TaskID), spec.HandlerID)
}
