package sandbox

import (
	"encoding/json"
	"fmt"
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
func ShimExec(command string, args []string) (*ExecResponse, error) {
	cwd, _ := os.Getwd()
	token := os.Getenv("BOID_BROKER_TOKEN")

	req := ExecRequest{
		Command:   command,
		Args:      args,
		Cwd:       cwd,
		Token:     token,
		Streaming: true,
	}
	return sendStreamingExecRequest(req)
}

// sendStreamingExecRequest connects to the broker, sends req, and reads the
// resulting StreamChunks. stdout/stderr chunks are forwarded to os.Stdout /
// os.Stderr in real-time. When the shim receives SIGINT/SIGTERM/SIGHUP it
// sends a kill chunk so the broker can terminate the host process group.
//
// No longer takes an explicit brokerSocket parameter (docs/plans/
// phase6-cutover-followups.md §⓪): brokerclient.DialFromEnv picks UNIX vs
// TCP+mTLS from this process's own environment, the same decision
// sendExecRequest's SendJSONFromEnv makes for the one-shot JSON path — a
// container-backend job's host-command exec (git/gh/docker/... via a
// /run/boid/bin/<name> shim symlink) needs this exactly as much as `boid
// task update` does; leaving this streaming path UNIX-only would have left
// every host command broken under the container backend even after the
// rest of this gap was closed.
func sendStreamingExecRequest(req ExecRequest) (*ExecResponse, error) {
	conn, err := brokerclient.DialFromEnv()
	if err != nil {
		return nil, err
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

// sendExecRequest is a thin wrapper over brokerclient.SendJSONFromEnv, which
// holds the shared transport-selection + dial/encode/decode logic (also
// used by the go-native sandbox runner's broker job-done path via
// brokerclient.JobDone). No longer takes an explicit brokerSocket
// parameter (docs/plans/phase6-cutover-followups.md §⓪): the shim is
// always a real subprocess, so its own os.Environ() already carries
// whichever of BOID_BROKER_SOCKET / BOID_BROKER_TLS_* the sandbox_builder/
// container_backend wiring set, and SendJSONFromEnv is the single place
// that decision is made — every caller here (RunBoidShim and its
// sub-dispatchers) used to thread a brokerSocket string through several
// layers purely to hand it to this one call site; dropping the parameter
// removes that plumbing entirely rather than leaving it to duplicate
// SendJSONFromEnv's own env lookup.
func sendExecRequest(req ExecRequest) (*ExecResponse, error) {
	var resp ExecResponse
	if err := brokerclient.SendJSONFromEnv(&req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
