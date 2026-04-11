package kit

import (
	"fmt"
	"os/exec"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// RequirementError records a command that is missing from PATH for a specific kit.
type RequirementError struct {
	KitName string
	Command string
}

// Error implements the error interface.
func (e RequirementError) Error() string {
	if e.KitName != "" {
		return fmt.Sprintf("kit %q: command %q not found in PATH", e.KitName, e.Command)
	}
	return fmt.Sprintf("command %q not found in PATH", e.Command)
}

// ValidateRequirements checks that every command declared in kit.requires.commands
// is present in PATH. It returns one RequirementError per missing command.
func ValidateRequirements(kits []orchestrator.KitMeta) []RequirementError {
	var errs []RequirementError
	for _, k := range kits {
		if k.Requires == nil {
			continue
		}
		kitName := ""
		if k.Meta != nil {
			kitName = k.Meta.Name
		}
		for _, cmd := range k.Requires.Commands {
			if _, err := exec.LookPath(cmd); err != nil {
				errs = append(errs, RequirementError{KitName: kitName, Command: cmd})
			}
		}
	}
	return errs
}
