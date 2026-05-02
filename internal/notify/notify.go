// Package notify exec's a user-configured command for agent-driven
// notifications. Triggered explicitly by `boid task notify` calls; no
// payload diffing or polling.
package notify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Service exec's the configured Command for each notification event.
// A nil receiver or empty Command makes Notify a no-op so the rest of
// the system does not need to special-case "notifications disabled".
type Service struct {
	Command   []string
	PublicURL string
}

// Notify exec's the service's Command synchronously, passing event data
// through environment variables. Returns an error if the command fails
// to start or exits non-zero. Callers should treat the error as
// notification-side failure (the agent's intent is recorded by the
// calling boid task notify regardless).
func (s *Service) Notify(ctx context.Context, taskID, projectID, message string) error {
	if s == nil || len(s.Command) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, s.Command[0], s.Command[1:]...)
	cmd.Env = append(os.Environ(),
		"BOID_TASK_ID="+taskID,
		"BOID_PROJECT_ID="+projectID,
		"BOID_MESSAGE="+message,
		"BOID_TASK_URL="+s.taskURL(taskID),
	)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("notify command: %w", err)
	}
	return nil
}

func (s *Service) taskURL(taskID string) string {
	if s.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(s.PublicURL, "/") + "/tasks/" + taskID
}
