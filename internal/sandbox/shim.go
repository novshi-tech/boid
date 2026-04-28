package sandbox

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

func CommandFromArgv0(argv0 string) string {
	return filepath.Base(argv0)
}

// shimBinaryPath returns the absolute path of the shim binary as it appears
// inside the sandbox. For host commands this equals the bind-mount target
// (e.g. /usr/bin/gh, /home/user/proj/e2e/run.sh), which is the canonical key
// the broker uses to identify the requested host command. The fallback to
// argv0 covers exotic environments where /proc/self/exe is unavailable.
func shimBinaryPath(argv0 string) string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return exe
	}
	if filepath.IsAbs(argv0) {
		return argv0
	}
	if abs, err := filepath.Abs(argv0); err == nil {
		return abs
	}
	return argv0
}

func ShimExec(brokerSocket, argv0 string, args []string, stdin []byte) (*ExecResponse, error) {
	cwd, _ := os.Getwd()
	token := os.Getenv("BOID_BROKER_TOKEN")

	req := ExecRequest{
		Command: shimBinaryPath(argv0),
		Args:    args,
		Cwd:     cwd,
		Token:   token,
		Stdin:   stdin,
	}
	return sendExecRequest(brokerSocket, req)
}

func sendExecRequest(brokerSocket string, req ExecRequest) (*ExecResponse, error) {
	conn, err := net.Dial("unix", brokerSocket)
	if err != nil {
		return nil, fmt.Errorf("connect to broker: %w", err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	var resp ExecResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	return &resp, nil
}
