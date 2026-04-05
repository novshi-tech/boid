package tui

import (
	"os"
	"os/exec"
	"strings"
)

// InTmux reports whether the process is running inside a tmux session.
func InTmux() bool {
	return os.Getenv("TMUX") != ""
}

// OpenJobInPane opens the given job in a new horizontal tmux split.
// Returns the pane ID of the newly created pane.
func OpenJobInPane(jobID string) (string, error) {
	cmd := exec.Command("tmux", "split-window", "-h", "-P", "-F", "#{pane_id}", "boid", "job", "attach", jobID)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// PaneAlive reports whether the given tmux pane still exists.
func PaneAlive(paneID string) bool {
	if paneID == "" {
		return false
	}
	cmd := exec.Command("tmux", "list-panes", "-a", "-F", "#{pane_id}")
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == paneID {
			return true
		}
	}
	return false
}

// FocusPane focuses an existing tmux pane.
func FocusPane(paneID string) error {
	return exec.Command("tmux", "select-pane", "-t", paneID).Run()
}
