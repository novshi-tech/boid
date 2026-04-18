package dispatcher

import "os"

// hostHomeDir returns the sandbox HOME — always the current user's real home
// directory. Using the real HOME keeps path conventions consistent inside and
// outside the sandbox (tools assume ~ == /home/<user>, or wherever the user
// actually lives). Isolation is provided by the HOME tmpfs mount stacked on
// top of it, which hides host files from the sandboxed process.
func hostHomeDir() string {
	if dir, err := os.UserHomeDir(); err == nil && dir != "" {
		return dir
	}
	// Fallback: UserHomeDir() is documented to always succeed on Linux, but
	// degrade gracefully if HOME is unset in some minimal environment.
	return "/root"
}
