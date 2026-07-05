package sandbox

import (
	"fmt"
	"os"
)

// RunGitShim forwards the raw `git` argv to the broker over the shared
// broker transport (sendExecRequest). Classification (direct vs. brokered),
// argv parsing, and policy checks all happen host-side in git_builtin.go —
// the shim itself does no interpretation of args.
func RunGitShim(args []string) (int, error) {
	brokerSocket := os.Getenv("BOID_BROKER_SOCKET")
	if brokerSocket == "" {
		return 1, fmt.Errorf("boid shim: BOID_BROKER_SOCKET not set")
	}

	cwd, _ := os.Getwd()
	req := ExecRequest{
		Command: shimBinaryPath(os.Args[0]),
		Args:    append([]string(nil), args...),
		Cwd:     cwd,
		Token:   os.Getenv("BOID_BROKER_TOKEN"),
	}
	resp, err := sendExecRequest(brokerSocket, req)
	if err != nil {
		return 1, fmt.Errorf("boid shim: %w", err)
	}
	if resp.Stdout != "" {
		_, _ = os.Stdout.WriteString(resp.Stdout)
	}
	if resp.Stderr != "" {
		_, _ = os.Stderr.WriteString(resp.Stderr)
	}
	return resp.ExitCode, nil
}
