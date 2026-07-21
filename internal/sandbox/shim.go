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

// CommandFromArgv0 returns the declared short (host_commands.<name>) name for
// this shim invocation, derived from its argv[0]'s basename. As of the 5a-3
// cutover (docs/plans/phase5-shim-and-task-context.md, "5a: shim 固定ディレク
// トリ化" PR3) every shim's bind-mount basename == its declared short name by
// construction (sandbox_builder's sandboxShimBinDir + hostCommandSymlinks
// materialize `/run/boid/bin/<name>` symlinks pointing at the boid multi-call
// binary), so the basename is always a trustworthy source of truth. The
// pre-5a-3 BOID_HOST_COMMAND_NAMES env-map / ResolveShimCommandName escape
// hatch for aliased host_commands.<name>.path entries was retired in the same
// change — the alias case now resolves at the dispatcher (symlink named after
// the declared name), not inside the shim.
func CommandFromArgv0(argv0 string) string {
	return filepath.Base(argv0)
}

// ShimExec sends a host command request to the broker using the streaming
// protocol so that stdout/stderr is forwarded in real-time and signals
// (Ctrl-C) are propagated to the host process group.
//
// command is the declared short name from CommandFromArgv0 — as of 5a-3
// (docs/plans/phase5-shim-and-task-context.md) the shim's bind-mount basename
// always equals its declared short name, so this is what the broker's
// Commands map is keyed by (dispatcher.ResolveHostCommands' byName view). The
// pre-5a-3 absolute-bind-mount-path Command shape and the broker's
// corresponding Path-scan fallback were both retired in the same change.
func ShimExec(brokerSocket, command string, args []string) (*ExecResponse, error) {
	cwd, _ := os.Getwd()
	token := os.Getenv("BOID_BROKER_TOKEN")

	req := ExecRequest{
		Command:   command,
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
