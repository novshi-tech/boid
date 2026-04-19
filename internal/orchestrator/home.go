package orchestrator

import "os"

// sandboxHomeDir returns the HOME directory the sandbox exposes to jobs.
// Mirrors dispatcher.hostHomeDir but kept local so orchestrator stays
// independent of dispatcher (layer boundary).
func sandboxHomeDir() string {
	if dir, err := os.UserHomeDir(); err == nil && dir != "" {
		return dir
	}
	return "/root"
}
