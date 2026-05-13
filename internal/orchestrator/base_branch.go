package orchestrator

import (
	"fmt"
	"os"
	"strings"
)

// taskBaseBranchVarName is the only template variable expanded by
// ExpandTaskBaseBranch. See ExpandTaskBaseBranch for the contract.
const taskBaseBranchVarName = "TASK_REMOTE_ID"

// currentBranchVarName is the project-context template variable expanded by
// ExpandBaseBranch (defined in branch_var.go). ExpandTaskBaseBranch must leave
// it untouched so callers can compose the two expanders.
const currentBranchVarName = "current_branch"

// ExpandTaskBaseBranch expands the ${TASK_REMOTE_ID} template variable in
// template against the supplied remoteID. Other template variables are
// rejected with one specific carve-out: ${current_branch} (the variable owned
// by ExpandBaseBranch) is preserved as-is so callers can compose the two
// expanders without ordering surprises.
//
// Contract:
//   - Static values (no ${...} or $... markers, or only ${current_branch}) pass
//     through unchanged. An empty remoteID is fine in that case.
//   - If the template references ${TASK_REMOTE_ID} but remoteID is empty,
//     expansion fails: task creation must not silently produce a broken branch
//     name. Callers are expected to surface this as a 400.
//   - Any other ${VAR} or $VAR reference is rejected. This intentionally
//     restricts the surface; the longer-term plan is to keep template
//     variables to an explicit allow-list rather than letting arbitrary env
//     vars leak into task state.
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
		case currentBranchVarName:
			// Leave ${current_branch} untouched for the project-context
			// expander to process.
			return "${" + name + "}"
		default:
			if expandErr == nil {
				expandErr = fmt.Errorf(
					"base_branch template %q references unknown variable ${%s}; only ${%s} and ${%s} are supported",
					template, name, taskBaseBranchVarName, currentBranchVarName,
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
