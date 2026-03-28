package hook

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/novshi-tech/boid/internal/model"
)

type Runner interface {
	Execute(ctx context.Context, event *model.HookFireEvent) error
}

type Dispatcher struct {
	Runner   Runner
	MaxDepth int
}

func (d *Dispatcher) Dispatch(ctx context.Context, task *model.Task, hooks []model.Hook) error {
	return d.dispatch(ctx, task, hooks, 0)
}

func (d *Dispatcher) dispatch(ctx context.Context, task *model.Task, hooks []model.Hook, depth int) error {
	if depth >= d.MaxDepth {
		slog.Warn("hook dispatch depth limit reached", "depth", depth)
		return fmt.Errorf("hook dispatch depth limit reached (%d)", d.MaxDepth)
	}

	for _, h := range hooks {
		event := &model.HookFireEvent{
			EventID:   fmt.Sprintf("evt-%s-%s-%d", task.ID[:8], h.ID, depth),
			TaskID:    task.ID,
			ProjectID: task.ProjectID,
			Hook:      h,
		}

		if err := d.Runner.Execute(ctx, event); err != nil {
			slog.Error("hook execution failed", "hook_id", h.ID, "error", err)
			return fmt.Errorf("execute hook %q: %w", h.ID, err)
		}
	}
	return nil
}
