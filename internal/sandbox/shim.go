package sandbox

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/novshi-tech/boid/internal/sandbox/brokerclient"
)

func CommandFromArgv0(argv0 string) string {
	return filepath.Base(argv0)
}

// shimBinaryPath returns the absolute path of the shim binary as it appears
// inside the sandbox. For host commands this equals the bind-mount target
// (e.g. /usr/bin/gh, /home/user/proj/e2e/run.sh). The broker's policy table
// is keyed by short (declared) command name as of
// docs/plans/phase5-shim-and-task-context.md ("5a: shim 固定ディレクトリ化"
// PR1), but still accepts this absolute path as a compatibility fallback
// (see broker.go's lookupCommand) — 5a-2 is what switches ShimExec to send
// the short name (CommandFromArgv0) instead, at which point this function's
// result stops being meaningful to the broker. The fallback to argv0 covers
// exotic environments where /proc/self/exe is unavailable.
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

// ShimExec sends a host command request to the broker using the streaming
// protocol so that stdout/stderr is forwarded in real-time and signals
// (Ctrl-C) are propagated to the host process group.
func ShimExec(brokerSocket, argv0 string, args []string) (*ExecResponse, error) {
	cwd, _ := os.Getwd()
	token := os.Getenv("BOID_BROKER_TOKEN")

	req := ExecRequest{
		Command:   shimBinaryPath(argv0),
		Args:      args,
		Cwd:       cwd,
		Token:     token,
		Streaming: true,
	}
	return sendStreamingExecRequest(brokerSocket, req)
}

// sendStreamingExecRequest connects to the broker, sends req, and reads the
// resulting StreamChunks. stdout/stderr chunks are forwarded to os.Stdout /
// os.Stderr in real-time. When the shim receives SIGINT/SIGTERM/SIGHUP it
// sends a kill chunk so the broker can terminate the host process group.
func sendStreamingExecRequest(brokerSocket string, req ExecRequest) (*ExecResponse, error) {
	conn, err := net.Dial("unix", brokerSocket)
	if err != nil {
		return nil, fmt.Errorf("connect to broker: %w", err)
	}
	defer conn.Close()

	enc := json.NewEncoder(conn)
	if err := enc.Encode(&req); err != nil {
		return nil, fmt.Errorf("send request: %w", err)
	}

	// Forward OS signals to the host process via a kill chunk.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	done := make(chan struct{})
	go func() {
		defer signal.Stop(sigCh)
		select {
		case <-sigCh:
			// Best-effort: ignore encode error (conn may already be closing).
			_ = enc.Encode(&StreamChunk{Type: StreamTypeKill})
		case <-done:
		}
	}()
	defer close(done)

	dec := json.NewDecoder(conn)
	exitCode := 0
	for {
		var chunk StreamChunk
		if err := dec.Decode(&chunk); err != nil {
			break
		}
		switch chunk.Type {
		case StreamTypeStdout:
			_, _ = os.Stdout.WriteString(chunk.Data)
		case StreamTypeStderr:
			_, _ = os.Stderr.WriteString(chunk.Data)
		case StreamTypeExit:
			exitCode = chunk.ExitCode
		}
		if chunk.Type == StreamTypeExit {
			break
		}
	}

	return &ExecResponse{ExitCode: exitCode}, nil
}

// sendExecRequest is a thin wrapper over brokerclient.SendJSON, which holds the
// shared dial/encode/decode transport (also used by the go-native sandbox
// runner's broker job-done path).
func sendExecRequest(brokerSocket string, req ExecRequest) (*ExecResponse, error) {
	var resp ExecResponse
	if err := brokerclient.SendJSON(brokerSocket, &req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
