package dispatcher_test

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/novshi-tech/boid/internal/dispatcher"
)

type statefulRuntime struct {
	mu       sync.Mutex
	nextID   int
	starts   map[string]dispatcher.RuntimeStartSpec
	stopped  []string
	handles  []dispatcher.RuntimeHandle
	startErr error
	stopErr  error
}

func newStatefulRuntime() *statefulRuntime {
	return &statefulRuntime{starts: make(map[string]dispatcher.RuntimeStartSpec)}
}

func (r *statefulRuntime) Start(_ context.Context, spec dispatcher.RuntimeStartSpec) (*dispatcher.RuntimeHandle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.startErr != nil {
		return nil, r.startErr
	}

	handle := dispatcher.RuntimeHandle{
		ID:          fmt.Sprintf("runtime-%d", r.nextID),
		Interactive: spec.Interactive,
		TTY:         spec.TTY,
	}
	r.nextID++
	if len(r.handles) > 0 {
		handle = r.handles[0]
		r.handles = r.handles[1:]
	}
	r.starts[handle.ID] = spec
	return &handle, nil
}

func (r *statefulRuntime) Attach(_ context.Context, _ string, _ dispatcher.RuntimeAttachRequest) error {
	return dispatcher.ErrRuntimeUnsupported
}

func (r *statefulRuntime) Resize(_ context.Context, _ string, _ dispatcher.TerminalSize) error {
	return dispatcher.ErrRuntimeUnsupported
}

func (r *statefulRuntime) Wait(_ context.Context, _ string) (dispatcher.RuntimeExit, error) {
	return dispatcher.RuntimeExit{}, dispatcher.ErrRuntimeUnsupported
}

func (r *statefulRuntime) Stop(_ context.Context, runtimeID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stopErr != nil {
		return r.stopErr
	}
	delete(r.starts, runtimeID)
	r.stopped = append(r.stopped, runtimeID)
	return nil
}

func (r *statefulRuntime) ActiveRuntimeIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	ids := make([]string, 0, len(r.starts))
	for id := range r.starts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (r *statefulRuntime) StoppedRuntimeIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	ids := append([]string(nil), r.stopped...)
	sort.Strings(ids)
	return ids
}

func (r *statefulRuntime) StartSpec(runtimeID string) (dispatcher.RuntimeStartSpec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	spec, ok := r.starts[runtimeID]
	return spec, ok
}
