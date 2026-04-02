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

func ShimExec(brokerSocket, command string, args []string, stdin []byte) (*ExecResponse, error) {
	cwd, _ := os.Getwd()
	token := os.Getenv("BOID_BROKER_TOKEN")

	req := ExecRequest{
		Command: command,
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
