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
	"sync"
)

// Service exec's the configured Command for each notification event.
// A nil receiver or empty Command makes Notify a no-op so the rest of
// the system does not need to special-case "notifications disabled".
//
// Command/PublicURL are guarded by mu (docs/plans/volume-only-daemon.md
// §論点 f: `notify.command`/`web.public_url` are both classified "dynamic"
// — silently hot-reloadable via `boid config set/apply/edit`). Every
// pre-this-PR caller constructs a Service once at daemon startup and never
// touches these fields again, so the zero-value mutex is safe to add
// without changing any existing call site's behavior; internal/server's
// config-apply path is the only new caller of Update.
type Service struct {
	mu        sync.RWMutex
	Command   []string
	PublicURL string
}

// Update swaps Command/PublicURL live, under mu — the daemon-side half of
// hot-reloading notify.command/web.public_url (internal/server's
// ApplyConfigYAML). Safe to call concurrently with Notify.
func (s *Service) Update(command []string, publicURL string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Command = append([]string(nil), command...)
	s.PublicURL = publicURL
}

// snapshot returns a consistent (Command, PublicURL) pair under a single
// lock acquisition, so Notify never observes a torn update (a new Command
// paired with the old PublicURL, or vice versa) from a concurrent Update.
func (s *Service) snapshot() ([]string, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Command, s.PublicURL
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
	// URLPath, when non-empty, overrides the default BOID_TASK_URL target
	// resolution. The caller supplies a full path (e.g. "/tasks/abc/questions/q1")
	// and Notify prefixes it with PublicURL. Used by ask-mode notifications
	// so the user opens the Q&A page directly instead of the job terminal.
	URLPath string
}

// Notify exec's the service's Command synchronously, passing event data
// through environment variables. Returns an error if the command fails
// to start or exits non-zero. Callers should treat the error as
// notification-side failure (the agent's intent is recorded by the
// calling boid task notify regardless).
func (s *Service) Notify(ctx context.Context, ev Event) error {
	if s == nil {
		return nil
	}
	command, publicURL := s.snapshot()
	if len(command) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Env = append(os.Environ(),
		"BOID_TASK_ID="+ev.TaskID,
		"BOID_TASK_TITLE="+ev.TaskTitle,
		"BOID_PROJECT_ID="+ev.ProjectID,
		"BOID_PROJECT_NAME="+ev.ProjectName,
		"BOID_MESSAGE="+ev.Message,
		"BOID_TASK_URL="+targetURL(publicURL, ev),
	)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("notify command: %w", err)
	}
	return nil
}

func targetURL(publicURL string, ev Event) string {
	if publicURL == "" {
		return ""
	}
	base := strings.TrimRight(publicURL, "/")
	if ev.URLPath != "" {
		if !strings.HasPrefix(ev.URLPath, "/") {
			return base + "/" + ev.URLPath
		}
		return base + ev.URLPath
	}
	if ev.JobID != "" {
		return base + "/jobs/" + ev.JobID
	}
	return base + "/tasks/" + ev.TaskID
}
