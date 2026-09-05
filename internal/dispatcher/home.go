package dispatcher

import "os"

// hostHomeDir returns the sandbox HOME — the current user's real home
// directory (isolation comes from a HOME tmpfs mount, not path substitution).
func hostHomeDir() string {
	if dir, err := os.UserHomeDir(); err == nil && dir != "" {
		return dir
	}
	return "/root"
}
