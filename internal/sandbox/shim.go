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
// (e.g. /usr/bin/gh, /home/user/proj/e2e/run.sh). This is no longer sent to
// the broker as ExecRequest.Command as of 5a-2 (see ResolveShimCommandName) —
// its remaining job is as the lookup key into BOID_HOST_COMMAND_NAMES, and as
// the broker-side compatibility fallback documented on broker.go's
// lookupCommand. The fallback to argv0 covers exotic environments where
// /proc/self/exe is unavailable.
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

// HostCommandNamesEnv is the name of the env var the dispatcher injects with
// a compact JSON map of {shim bind-mount path -> declared short name}
// (docs/plans/phase5-shim-and-task-context.md, "5a: shim 固定ディレクトリ化"
// PR2). It is built from the exact same dispatcher.ResolveHostCommands byPath
// map that hostCommandMounts binds each shim at (see sandbox_builder.go's
// buildHostCommandNamesEnv), so the mount-target → short-name association a
// shim instance resolves here is never a second, independently-maintained
// source of truth.
//
// This exists because CommandFromArgv0(argv0) (the file's basename) is not
// always the declared short name: host_commands.<name>.path can alias a
// command to a file whose basename differs from name (e.g.
// host_commands.run-e2e.path: e2e/run.sh — the sandbox-visible file is
// "run.sh", but the broker's policy table and BOID_HOST_COMMAND_RULES are
// keyed by "run-e2e"). Without this map, EarlyRejectFromEnv and ShimExec
// would both look up the wrong key for every aliased command.
const HostCommandNamesEnv = "BOID_HOST_COMMAND_NAMES"

// ResolveShimCommandName determines the short (declared) command name a host
// command shim invocation should identify itself as to the broker — both for
// the shim-side EarlyRejectFromEnv fast path and for ExecRequest.Command
// (ShimExec). It prefers an exact entry in BOID_HOST_COMMAND_NAMES keyed by
// this shim's own bind-mount path (shimBinaryPath) — the only reliable
// source when host_commands.<name>.path aliases the shim to a file whose
// basename differs from the declared name. It falls back to
// CommandFromArgv0(argv0) (the file's basename) when the env var is unset,
// empty, malformed, or has no entry for this exe — which is always correct
// for the common case where no alias is declared (basename already equals
// the short name).
func ResolveShimCommandName(argv0 string) string {
	if names, ok := parseHostCommandNames(os.Getenv(HostCommandNamesEnv)); ok {
		if name, ok := names[shimBinaryPath(argv0)]; ok && name != "" {
			return name
		}
	}
	return CommandFromArgv0(argv0)
}

// parseHostCommandNames decodes raw (the BOID_HOST_COMMAND_NAMES JSON
// payload) into a path->name map. Empty or malformed input yields (nil,
// false) so callers fall back to the basename, mirroring EarlyReject's
// treatment of BOID_HOST_COMMAND_RULES: this env var is a best-effort UX
// fast path, never a hard dependency the shim can crash or hang on.
func parseHostCommandNames(raw string) (map[string]string, bool) {
	if raw == "" {
		return nil, false
	}
	var names map[string]string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, false
	}
	return names, true
}

// ShimExec sends a host command request to the broker using the streaming
// protocol so that stdout/stderr is forwarded in real-time and signals
// (Ctrl-C) are propagated to the host process group.
//
// command is the resolved short (declared) name from ResolveShimCommandName,
// not the raw argv0 — as of 5a-2 (docs/plans/phase5-shim-and-task-context.md)
// this is what the broker's Commands map is keyed by (dispatcher.
// ResolveHostCommands' byName view). The broker still accepts the older
// absolute bind-mount path as a compatibility fallback (broker.go's
// lookupCommand) until 5a-3 drops it, but this shim no longer sends that
// form.
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
