package orchestrator

import (
	"fmt"
	"os"
	"strings"
)

// taskBaseBranchVarName is the only template variable expanded by
// ExpandTaskBaseBranch. See ExpandTaskBaseBranch for the contract.
const taskBaseBranchVarName = "TASK_REMOTE_ID"

// ExpandTaskBaseBranch expands the task remote ID in a workspace branch template.
func ExpandTaskBaseBranch(template, remoteID string) (string, error) {
	if !strings.Contains(template, "$") {
		return template, nil
	}

	var expandErr error
	result := os.Expand(template, func(name string) string {
		switch name {
		case taskBaseBranchVarName:
			if remoteID == "" {
				if expandErr == nil {
					expandErr = fmt.Errorf(
						"base_branch template %q requires ${%s} but task has no remote_id",
						template, taskBaseBranchVarName,
					)
				}
				return ""
			}
			return remoteID
		default:
			if expandErr == nil {
				expandErr = fmt.Errorf(
					"base_branch template %q references unknown variable ${%s}; only ${%s} is supported",
					template, name, taskBaseBranchVarName,
				)
			}
			return ""
		}
	})
	if expandErr != nil {
		return "", expandErr
	}
	return result, nil
}
