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

// Event carries the data passed to the notify command via env vars.
// All fields except TaskID and Message are best-effort: empty strings
// surface as empty env vars, which the user's script can ignore.
type Event struct {
	TaskID      string
	TaskTitle   string
	ProjectID   string
	ProjectName string
	Message     string
	JobID       string
}

// Notify exec's the service's Command synchronously, passing event data
// through environment variables. Returns an error if the command fails
// to start or exits non-zero. Callers should treat the error as
// notification-side failure (the agent's intent is recorded by the
// calling boid task notify regardless).
func (s *Service) Notify(ctx context.Context, ev Event) error {
	if s == nil || len(s.Command) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, s.Command[0], s.Command[1:]...)
	cmd.Env = append(os.Environ(),
		"BOID_TASK_ID="+ev.TaskID,
		"BOID_TASK_TITLE="+ev.TaskTitle,
		"BOID_PROJECT_ID="+ev.ProjectID,
		"BOID_PROJECT_NAME="+ev.ProjectName,
		"BOID_MESSAGE="+ev.Message,
		"BOID_TASK_URL="+s.targetURL(ev),
	)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("notify command: %w", err)
	}
	return nil
}

func (s *Service) targetURL(ev Event) string {
	if s.PublicURL == "" {
		return ""
	}
	base := strings.TrimRight(s.PublicURL, "/")
	if ev.JobID != "" {
		return base + "/jobs/" + ev.JobID + "/terminal"
	}
	return base + "/tasks/" + ev.TaskID
}
