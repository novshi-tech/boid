package sandbox

import (
	"os"
)

// runCardContextShim builds and sends the BoidRequest for `boid card
// context [--field F] [--format json|yaml]`. Unlike the task-context
// subcommands (boid_shim_task_context.go), there is no caller-supplied id at
// all: card/request identity comes only from the token entry the broker
// already holds (BoidOpCardContext's own doc comment), so this only needs
// to carry the shared --field/--format flags through.
//
// fullArgs is RunBoidShim's original args slice (["card", "context",
// ...flags]); trailing flags start at fullArgs[2:].
func runCardContextShim(fullArgs []string) (*ExecResponse, error) {
	field, format, err := parseTaskContextFlags(fullArgs[2:])
	if err != nil {
		return nil, err
	}

	req := &BoidRequest{
		Op:        BoidOpCardContext,
		TaskField: field,
	}

	cwd, _ := os.Getwd()
	execReq := ExecRequest{
		Command: os.Args[0],
		Args:    append([]string(nil), fullArgs...),
		Cwd:     cwd,
		Token:   os.Getenv("BOID_BROKER_TOKEN"),
		Boid:    req,
	}

	resp, err := sendExecRequest(execReq)
	if err != nil || resp == nil {
		return resp, err
	}
	// Same re-render rule as runTaskContextShim: only the full-object,
	// successful reply is ever converted, never a --field scalar or an
	// error message in Stderr.
	if field == "" && format == "yaml" && resp.ExitCode == 0 {
		resp.Stdout = jsonToYAMLForShim(resp.Stdout)
	}
	return resp, nil
}
