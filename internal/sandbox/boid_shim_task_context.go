package sandbox

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// taskContextOps maps the Phase 5b PR1 task-context subcommand names
// (docs/plans/phase5-shim-and-task-context.md) to their BoidOp. Checked by
// RunBoidShim before falling through to the generic parseBoidRequest path.
var taskContextOps = map[string]BoidOp{
	"current":      BoidOpTaskCurrent,
	"instructions": BoidOpTaskInstructions,
	"env":          BoidOpTaskEnv,
	"payload":      BoidOpTaskPayload,
}

// runTaskContextShim builds and sends the BoidRequest for one of the four
// task-context subcommands, then — for a successful, --field-less response —
// re-renders the broker's canonical-JSON reply as YAML when requested.
//
// fullArgs is RunBoidShim's original args slice (["task", "<subcommand>",
// ...flags]); trailing flags start at fullArgs[2:].
func runTaskContextShim(op BoidOp, fullArgs []string, brokerSocket string) (*ExecResponse, error) {
	field, format, err := parseTaskContextFlags(fullArgs[2:])
	if err != nil {
		return nil, err
	}

	req := &BoidRequest{
		Op:        op,
		TaskID:    os.Getenv("BOID_TASK_ID"),
		JobID:     os.Getenv("BOID_JOB_ID"),
		TaskField: field,
	}

	cwd, _ := os.Getwd()
	execReq := ExecRequest{
		// req.Boid != nil so the broker routes on the typed payload; Command
		// is informational only (surfaces in diagnostic logs). Preserve
		// os.Args[0] so the log entry names the actual invocation shape.
		Command: os.Args[0],
		Args:    append([]string(nil), fullArgs...),
		Cwd:     cwd,
		Token:   os.Getenv("BOID_BROKER_TOKEN"),
		Boid:    req,
	}

	resp, err := sendExecRequest(brokerSocket, execReq)
	if err != nil || resp == nil {
		return resp, err
	}
	// --field replies are already the plain-text scalar task_get established
	// (a JSON/YAML choice is meaningless there); only the full-object form
	// gets re-rendered, and only on success (an error message in Stderr is
	// left untouched regardless of format).
	if field == "" && format == "yaml" && resp.ExitCode == 0 {
		resp.Stdout = jsonToYAMLForShim(resp.Stdout)
	}
	return resp, nil
}

// parseTaskContextFlags parses the --field / --format flags shared by all
// four task-context subcommands. Defaults format to "yaml" — the CLI's
// closest echo of the file-based context it replaces (task.yaml,
// instructions.yaml, environment.yaml are all YAML; only payload has ever
// had a JSON sibling).
func parseTaskContextFlags(args []string) (field, format string, err error) {
	format = "yaml"
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--field" || strings.HasPrefix(arg, "--field="):
			value, next, ferr := takeStringFlagValue(args, i, "--field")
			if ferr != nil {
				return "", "", ferr
			}
			i = next
			field = value
		case arg == "--format" || strings.HasPrefix(arg, "--format="):
			value, next, ferr := takeStringFlagValue(args, i, "--format")
			if ferr != nil {
				return "", "", ferr
			}
			i = next
			if value != "json" && value != "yaml" {
				return "", "", fmt.Errorf("boid shim: --format must be \"json\" or \"yaml\" (got %q)", value)
			}
			format = value
		default:
			return "", "", fmt.Errorf("boid shim: unsupported flag %q", arg)
		}
	}
	return field, format, nil
}

// jsonToYAMLForShim converts a JSON string to YAML for display, falling back
// to the original string unchanged if it isn't valid JSON (defensive: a
// broker-side bug or an unexpected error string should never crash the CLI
// on the render step).
func jsonToYAMLForShim(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	out, err := yaml.Marshal(v)
	if err != nil {
		return s
	}
	return string(out)
}
