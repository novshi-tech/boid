//go:build linux

package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// handleStreamingExec routes a streaming ExecRequest.
// The boid builtin is handled synchronously via Handle and converted to
// stream format. Regular host commands are executed with a PTY for stdout
// so the child process sees a TTY and uses line buffering (D-1).
func (b *Broker) handleStreamingExec(conn net.Conn, req *ExecRequest) {
	// Boid builtin is fast; re-use the non-streaming path.
	if req.Boid != nil {
		resp := b.Handle(req)
		sendStreamResponse(conn, resp)
		return
	}

	b.mu.RLock()
	entry, ok := b.registry[req.Token]
	b.mu.RUnlock()
	if !ok {
		sendStreamError(conn, "invalid token", 1)
		return
	}

	// lookupCommand covers both the canonical short-name key and the
	// staging-period absolute-path fallback (see its doc comment in
	// broker.go); this is the path ShimExec actually takes (Streaming=true),
	// so the fallback matters here, not just in the non-streaming Handle().
	def, ok := lookupCommand(entry.Commands, req.Command)
	if !ok {
		sendStreamError(conn, fmt.Sprintf("command not allowed: %s", filepath.Base(req.Command)), 1)
		return
	}

	b.execCommandStreaming(conn, req, def, entry)
}

// execCommandStreaming runs def.Path with PTY-based stdout (line buffering, D-1),
// a separate stderr pipe, process-group isolation (D-2), and an exit chunk for
// proper exit-code propagation (D-3).
func (b *Broker) execCommandStreaming(conn net.Conn, req *ExecRequest, def CommandDef, entry *tokenEntry) {
	if msg, ok := gateHostCommand(def, req.Args); !ok {
		sendStreamError(conn, msg, 1)
		return
	}

	binary := def.Path
	if binary == "" {
		var err error
		binary, err = exec.LookPath(def.Name)
		if err != nil {
			sendStreamError(conn, fmt.Sprintf("host_commands.%s: unable to locate %q in PATH: %v", def.Name, def.Name, err), 1)
			return
		}
	}

	cmd := exec.Command(binary, req.Args...)
	cmd.Dir = hostCommandCwd()

	// Build environment: inherit host env minus BOID_* internal markers,
	// then overlay def.Env. Always set TERM so child programs behave
	// correctly on the PTY.
	env := hostCommandEnv(def.Env)
	if !envContains(env, "TERM=") {
		env = append(env, "TERM=xterm-256color")
	}
	cmd.Env = env

	// Open PTY for stdout so bash (and similar shells) use line buffering.
	ptm, pts, ptyErr := openPTY()
	if ptyErr != nil {
		// PTY unavailable — fall back to non-streaming buffered execution.
		resp := b.execCommand(req, def, entry)
		sendStreamResponse(conn, resp)
		return
	}
	defer ptm.Close()
	setPTYSize(ptm, 220, 50)
	disablePTYOutputProcessing(pts)

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		pts.Close()
		sendStreamError(conn, fmt.Sprintf("stderr pipe: %v", err), 1)
		return
	}

	cmd.Stdout = pts
	// The broker never wires caller-provided stdin into the host process (see
	// ExecRequest doc comment). Connect /dev/null so bash doesn't hang on reads.
	devNull, nullErr := os.Open("/dev/null")
	if nullErr == nil {
		cmd.Stdin = devNull
		defer devNull.Close()
	}

	// Setsid: new session → new process group (PGID == PID).
	// Setctty: make pts the controlling terminal so isatty(1) returns true.
	// Ctty: fd 1 (stdout) in the child, which Go maps to pts.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    1,
	}

	if err := cmd.Start(); err != nil {
		pts.Close()
		_ = stderrPipe.Close()
		sendStreamError(conn, err.Error(), 1)
		return
	}
	pts.Close() // parent no longer needs slave FD

	pid := cmd.Process.Pid

	// Mutex guards concurrent JSON writes from the stdout/stderr goroutines.
	var encMu sync.Mutex
	enc := json.NewEncoder(conn)
	writeChunk := func(chunk StreamChunk) {
		encMu.Lock()
		defer encMu.Unlock()
		_ = enc.Encode(&chunk)
	}

	// Read kill chunks sent by the shim (D-2: signal propagation).
	go func() {
		dec := json.NewDecoder(conn)
		for {
			var chunk StreamChunk
			if err := dec.Decode(&chunk); err != nil {
				return
			}
			if chunk.Type == StreamTypeKill {
				killProcessGroup(pid, syscall.SIGTERM)
				return
			}
		}
	}()

	var wg sync.WaitGroup

	// Forward PTY master output as stdout chunks (line-buffered via PTY).
	// Strip ANSI/OSC escape sequences before forwarding: the PTY causes
	// programs like gh to emit terminal queries (OSC 11, CSI 6n) that corrupt
	// command substitution and JSON parsing in the sandbox.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := ptm.Read(buf)
			if n > 0 {
				data := stripANSIEscapes(string(buf[:n]))
				if data != "" {
					writeChunk(StreamChunk{Type: StreamTypeStdout, Data: data})
				}
			}
			if err != nil {
				// EIO is the normal EOF signal when the PTY slave closes.
				if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.EIO) {
					writeChunk(StreamChunk{Type: StreamTypeStderr, Data: err.Error()})
				}
				return
			}
		}
	}()

	// Forward stderr pipe as stderr chunks.
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 4096)
		for {
			n, err := stderrPipe.Read(buf)
			if n > 0 {
				writeChunk(StreamChunk{Type: StreamTypeStderr, Data: string(buf[:n])})
			}
			if err != nil {
				return
			}
		}
	}()

	wg.Wait()

	// Retrieve exit code (D-3: exit sync).
	code := 0
	if err := cmd.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
	}

	writeChunk(StreamChunk{Type: StreamTypeExit, ExitCode: code})
}

func sendStreamError(conn net.Conn, msg string, exitCode int) {
	enc := json.NewEncoder(conn)
	_ = enc.Encode(&StreamChunk{Type: StreamTypeStderr, Data: msg})
	_ = enc.Encode(&StreamChunk{Type: StreamTypeExit, ExitCode: exitCode})
}

func envContains(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
