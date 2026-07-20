//go:build linux

package sandbox_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// dialStreaming sends a streaming ExecRequest and collects all StreamChunks
// until the exit chunk arrives. Returns accumulated stdout, stderr and exit code.
func dialStreaming(t *testing.T, sockPath string, req sandbox.ExecRequest) (stdout, stderr string, exitCode int) {
	t.Helper()
	req.Streaming = true

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	dec := json.NewDecoder(conn)
	var sb, eb strings.Builder
	for {
		var chunk sandbox.StreamChunk
		if err := dec.Decode(&chunk); err != nil {
			t.Fatalf("decode chunk: %v", err)
		}
		switch chunk.Type {
		case sandbox.StreamTypeStdout:
			sb.WriteString(chunk.Data)
		case sandbox.StreamTypeStderr:
			eb.WriteString(chunk.Data)
		case sandbox.StreamTypeExit:
			return sb.String(), eb.String(), chunk.ExitCode
		}
	}
}

func startStreamingBroker(t *testing.T, cmds map[string]sandbox.CommandDef, ctx sandbox.TokenContext) (sockPath, token string) {
	t.Helper()
	bCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sockPath = filepath.Join(t.TempDir(), "broker.sock")
	b := &sandbox.Broker{SocketPath: sockPath}
	token = b.Register(cmds, nil, ctx)
	t.Cleanup(func() { b.Unregister(token) })
	if err := b.Start(bCtx); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	t.Cleanup(b.Stop)
	return sockPath, token
}

// TestBroker_StreamingOutputForwarding verifies that stdout/stderr chunks
// arrive and contain the expected content.
func TestBroker_StreamingOutputForwarding(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "hello.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'hello\\n'\nprintf 'world\\n' >&2\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	sockPath, token := startStreamingBroker(t,
		map[string]sandbox.CommandDef{
			script: {Name: "hello", Path: script, AllowedPatterns: []string{"*"}},
		},
		sandbox.TokenContext{JobID: "j1", TaskID: "t1", ProjectID: "p1", Role: "hook"},
	)

	stdout, stderr, code := dialStreaming(t, sockPath, sandbox.ExecRequest{
		Command: script,
		Token:   token,
	})

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "hello") {
		t.Errorf("stdout %q does not contain 'hello'", stdout)
	}
	if !strings.Contains(stderr, "world") {
		t.Errorf("stderr %q does not contain 'world'", stderr)
	}
}

// TestBroker_StreamingExitCode verifies that a non-zero exit code is
// correctly propagated through the streaming protocol.
func TestBroker_StreamingExitCode(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 42\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	sockPath, token := startStreamingBroker(t,
		map[string]sandbox.CommandDef{
			script: {Name: "fail", Path: script, AllowedPatterns: []string{"*"}},
		},
		sandbox.TokenContext{JobID: "j1", TaskID: "t1", ProjectID: "p1", Role: "hook"},
	)

	_, _, code := dialStreaming(t, sockPath, sandbox.ExecRequest{
		Command: script,
		Token:   token,
	})

	if code != 42 {
		t.Errorf("exit code = %d, want 42", code)
	}
}

// TestBroker_StreamingKillSignal sends a kill chunk while a long-running
// script is executing and verifies the process terminates quickly.
func TestBroker_StreamingKillSignal(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "sleep.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	bCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sockPath := filepath.Join(t.TempDir(), "broker.sock")
	b := &sandbox.Broker{SocketPath: sockPath}
	token := b.Register(map[string]sandbox.CommandDef{
		script: {Name: "sleep", Path: script, AllowedPatterns: []string{"*"}},
	}, nil, sandbox.TokenContext{JobID: "j2", TaskID: "t2", ProjectID: "p2", Role: "hook"})
	t.Cleanup(func() { b.Unregister(token) })
	if err := b.Start(bCtx); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	t.Cleanup(b.Stop)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req := sandbox.ExecRequest{
		Command:   script,
		Token:     token,
		Streaming: true,
	}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		t.Fatal(err)
	}

	// Give the process time to start, then send kill.
	time.Sleep(100 * time.Millisecond)
	kill := sandbox.StreamChunk{Type: sandbox.StreamTypeKill}
	if err := json.NewEncoder(conn).Encode(&kill); err != nil {
		t.Fatal(err)
	}

	// The broker should send an exit chunk shortly after the kill.
	done := make(chan int, 1)
	go func() {
		dec := json.NewDecoder(conn)
		for {
			var chunk sandbox.StreamChunk
			if err := dec.Decode(&chunk); err != nil {
				done <- -1
				return
			}
			if chunk.Type == sandbox.StreamTypeExit {
				done <- chunk.ExitCode
				return
			}
		}
	}()

	select {
	case <-done:
		// Process terminated — success.
	case <-time.After(5 * time.Second):
		t.Error("process did not terminate within 5s after kill chunk")
	}
}

// TestBroker_StreamingInvalidToken verifies that an invalid token causes a
// proper error exit chunk rather than hanging.
func TestBroker_StreamingInvalidToken(t *testing.T) {
	bCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sockPath := filepath.Join(t.TempDir(), "broker.sock")
	b := &sandbox.Broker{SocketPath: sockPath}
	if err := b.Start(bCtx); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	t.Cleanup(b.Stop)

	_, stderr, code := dialStreaming(t, sockPath, sandbox.ExecRequest{
		Command: "/bin/echo",
		Token:   "bad-token",
	})

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if stderr == "" {
		t.Error("expected non-empty stderr for invalid token")
	}
}

// TestBroker_StreamingBoidBuiltinFallback verifies that a Streaming=true
// request with a boid payload still works (converted to stream format).
func TestBroker_StreamingBoidBuiltinFallback(t *testing.T) {
	fakeExec := &fakeBoidExecutor{
		resp: &sandbox.ExecResponse{ExitCode: 0, Stdout: "boid-ok\n"},
	}
	bCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	sockPath := filepath.Join(t.TempDir(), "broker.sock")
	projectDir := t.TempDir()
	b := &sandbox.Broker{SocketPath: sockPath, BoidExecutor: fakeExec}
	token := b.Register(map[string]sandbox.CommandDef{}, testHookBoidPolicies(), sandbox.TokenContext{
		JobID:      "j3",
		TaskID:     "t3",
		ProjectID:  "p3",
		Role:       "hook",
		ProjectDir: projectDir,
	})
	t.Cleanup(func() { b.Unregister(token) })
	if err := b.Start(bCtx); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	t.Cleanup(b.Stop)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req := sandbox.ExecRequest{
		Command:   "boid",
		Cwd:       projectDir,
		Token:     token,
		Streaming: true,
		Boid: &sandbox.BoidRequest{
			Op:    sandbox.BoidOpJobDone,
			JobID: "j3",
		},
	}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		t.Fatal(err)
	}

	// Read all chunks.
	dec := json.NewDecoder(conn)
	var sb strings.Builder
	code := -1
	for {
		var chunk sandbox.StreamChunk
		if err := dec.Decode(&chunk); err != nil {
			break
		}
		if chunk.Type == sandbox.StreamTypeStdout {
			sb.WriteString(chunk.Data)
		}
		if chunk.Type == sandbox.StreamTypeExit {
			code = chunk.ExitCode
			break
		}
	}

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(sb.String(), "boid-ok") {
		t.Errorf("stdout %q does not contain 'boid-ok'", sb.String())
	}
}

// TestBroker_StreamingCommandNotAllowed verifies that a disallowed command
// results in a proper error exit chunk.
func TestBroker_StreamingCommandNotAllowed(t *testing.T) {
	sockPath, token := startStreamingBroker(t,
		map[string]sandbox.CommandDef{
			"/bin/echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
		},
		sandbox.TokenContext{JobID: "j4", TaskID: "t4", ProjectID: "p4", Role: "hook"},
	)

	_, stderr, code := dialStreaming(t, sockPath, sandbox.ExecRequest{
		Command: "/bin/rm",
		Token:   token,
	})

	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "not allowed") {
		t.Errorf("stderr %q should contain 'not allowed'", stderr)
	}
}

// TestBroker_StreamingShortNameKeyedCommand covers the real production path
// for host commands (docs/plans/phase5-shim-and-task-context.md, "5a: shim
// 固定ディレクトリ化" PR1): sandbox.ShimExec always sets Streaming=true, so
// handleStreamingExec — not the non-streaming Handle path — is what actually
// resolves ExecRequest.Command for a live shim call. This pins down that the
// short-name canonical key resolves here too.
func TestBroker_StreamingShortNameKeyedCommand(t *testing.T) {
	sockPath, token := startStreamingBroker(t,
		map[string]sandbox.CommandDef{
			"echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
		},
		sandbox.TokenContext{JobID: "j-sn", TaskID: "t-sn", ProjectID: "p-sn", Role: "hook"},
	)

	stdout, _, code := dialStreaming(t, sockPath, sandbox.ExecRequest{
		Command: "echo",
		Args:    []string{"hello"},
		Token:   token,
	})

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "hello") {
		t.Errorf("stdout %q does not contain 'hello'", stdout)
	}
}

// TestBroker_StreamingAbsolutePathFallback is the streaming-path counterpart
// of the same staging-period compatibility fallback: entry.Commands is now
// short-name keyed, but the shim (before 5a-2) still sends the absolute
// bind-mount path as ExecRequest.Command. handleStreamingExec must resolve
// it via the same Path-match fallback lookupCommand implements.
func TestBroker_StreamingAbsolutePathFallback(t *testing.T) {
	sockPath, token := startStreamingBroker(t,
		map[string]sandbox.CommandDef{
			"echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
		},
		sandbox.TokenContext{JobID: "j-fb", TaskID: "t-fb", ProjectID: "p-fb", Role: "hook"},
	)

	stdout, _, code := dialStreaming(t, sockPath, sandbox.ExecRequest{
		Command: "/bin/echo",
		Args:    []string{"hello"},
		Token:   token,
	})

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if !strings.Contains(stdout, "hello") {
		t.Errorf("stdout %q does not contain 'hello'", stdout)
	}
}

// TestBroker_StreamingBackwardCompatibility verifies that non-streaming
// requests (Streaming=false) still work with the old single-response protocol.
func TestBroker_StreamingBackwardCompatibility(t *testing.T) {
	sockPath, token := startStreamingBroker(t,
		map[string]sandbox.CommandDef{
			"/bin/echo": {Name: "echo", Path: "/bin/echo", AllowedPatterns: []string{"*"}},
		},
		sandbox.TokenContext{JobID: "j5", TaskID: "t5", ProjectID: "p5", Role: "hook"},
	)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req := sandbox.ExecRequest{
		Command: "/bin/echo",
		Args:    []string{"compat"},
		Token:   token,
		// Streaming intentionally omitted (false)
	}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		t.Fatal(err)
	}

	var resp sandbox.ExecResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", resp.ExitCode)
	}
	if !strings.Contains(resp.Stdout, "compat") {
		t.Errorf("stdout %q does not contain 'compat'", resp.Stdout)
	}
}

// TestBroker_StreamingMultiLineOutput verifies that a script printing multiple
// lines delivers all lines (tests real-time forwarding completeness).
func TestBroker_StreamingMultiLineOutput(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "lines.sh")
	content := "#!/bin/sh\n"
	want := make([]string, 20)
	for i := range want {
		want[i] = "line" + string(rune('0'+i))
		content += "printf '" + want[i] + "\\n'\n"
	}
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	sockPath, token := startStreamingBroker(t,
		map[string]sandbox.CommandDef{
			script: {Name: "lines", Path: script, AllowedPatterns: []string{"*"}},
		},
		sandbox.TokenContext{JobID: "j6", TaskID: "t6", ProjectID: "p6", Role: "hook"},
	)

	stdout, _, code := dialStreaming(t, sockPath, sandbox.ExecRequest{
		Command: script,
		Token:   token,
	})

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	for _, line := range want {
		if !strings.Contains(stdout, line) {
			t.Errorf("stdout missing line %q; stdout=%q", line, stdout)
		}
	}
}

// TestBroker_StreamingANSIStripped verifies that ANSI/OSC escape sequences
// emitted by a command on the PTY are stripped before being forwarded as
// stdout chunks. This covers the gh terminal-query corruption bug (#559):
// OSC background-color queries and CSI sequences must not appear in output
// captured by $(...) or JSON parsers.
func TestBroker_StreamingANSIStripped(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "gh_mock.sh")
	// Simulate the kind of output gh emits when it detects a terminal:
	//   OSC 11 background-color query (BEL terminated)
	//   CSI cursor-position query (DSR)
	//   The actual useful output (a PR number)
	content := "#!/bin/sh\n" +
		`printf '\033]11;?\007'` + "\n" + // OSC background query
		`printf '\033[6n'` + "\n" + // CSI cursor position query
		`printf '42\n'` + "\n" // real output
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}

	sockPath, token := startStreamingBroker(t,
		map[string]sandbox.CommandDef{
			script: {Name: "gh_mock", Path: script, AllowedPatterns: []string{"*"}},
		},
		sandbox.TokenContext{JobID: "j8", TaskID: "t8", ProjectID: "p8", Role: "hook"},
	)

	stdout, _, code := dialStreaming(t, sockPath, sandbox.ExecRequest{
		Command: script,
		Token:   token,
	})

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	// Escape sequences must be gone.
	if strings.Contains(stdout, "\x1b") {
		t.Errorf("stdout contains ESC after stripping: %q", stdout)
	}
	// The real output must survive.
	if !strings.Contains(stdout, "42") {
		t.Errorf("stdout %q does not contain expected value '42'", stdout)
	}
}

// TestBroker_StreamingNeverWiresStdin verifies that the streaming host-command
// path never connects caller-supplied stdin to the host process: `cat` runs
// with stdin detached (connected to /dev/null), exits immediately with 0, and
// produces no stdout, instead of hanging or echoing any input.
func TestBroker_StreamingNeverWiresStdin(t *testing.T) {
	sockPath, token := startStreamingBroker(t,
		map[string]sandbox.CommandDef{
			"/bin/cat": {Name: "cat", Path: "/bin/cat", AllowedPatterns: []string{"*"}},
		},
		sandbox.TokenContext{JobID: "j7", TaskID: "t7", ProjectID: "p7", Role: "hook"},
	)

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	req := sandbox.ExecRequest{
		Command:   "/bin/cat",
		Token:     token,
		Streaming: true,
	}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		t.Fatal(err)
	}

	dec := json.NewDecoder(conn)
	var buf bytes.Buffer
	code := -1
	for {
		var chunk sandbox.StreamChunk
		if err := dec.Decode(&chunk); err != nil {
			break
		}
		if chunk.Type == sandbox.StreamTypeStdout {
			buf.WriteString(chunk.Data)
		}
		if chunk.Type == sandbox.StreamTypeExit {
			code = chunk.ExitCode
			break
		}
	}

	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if buf.String() != "" {
		t.Errorf("stdout = %q, want empty (stdin never wired)", buf.String())
	}
}
