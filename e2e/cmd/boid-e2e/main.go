package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/novshi-tech/boid/e2e/upstream"
	"github.com/novshi-tech/boid/internal/client"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usageError("command required")
	}

	switch args[0] {
	case "wait-unix-socket":
		return runWaitUnixSocket(args[1:])
	case "wait-health":
		return runWaitHealth(args[1:])
	case "get-task":
		return runGetTask(args[1:])
	case "wait-task-status":
		return runWaitTaskStatus(args[1:])
	case "list-jobs":
		return runListJobs(args[1:])
	case "wait-job-count":
		return runWaitJobCount(args[1:])
	case "assert-job-role-count":
		return runAssertJobRoleCount(args[1:])
	case "ws-job-output":
		return runWSJobOutput(args[1:])
	case "fake-docker":
		return runFakeDocker(args[1:])
	case "upstream-serve":
		return runUpstreamServe(args[1:])
	default:
		return usageError(fmt.Sprintf("unknown command %q", args[0]))
	}
}

func runWaitUnixSocket(args []string) error {
	fs := flag.NewFlagSet("wait-unix-socket", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	timeout := fs.Duration("timeout", 15*time.Second, "maximum wait time")
	interval := fs.Duration("interval", 100*time.Millisecond, "poll interval")

	if err := fs.Parse(args); err != nil {
		return err
	}

	socketPath := resolveSocketPath(fs.Args())
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := waitUnixSocket(ctx, socketPath, *interval); err != nil {
		return fmt.Errorf("wait unix socket %s: %w", socketPath, err)
	}
	return nil
}

func runWaitHealth(args []string) error {
	fs := flag.NewFlagSet("wait-health", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	timeout := fs.Duration("timeout", 15*time.Second, "maximum wait time")
	interval := fs.Duration("interval", 100*time.Millisecond, "poll interval")

	if err := fs.Parse(args); err != nil {
		return err
	}

	socketPath := resolveSocketPath(fs.Args())
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := waitHealth(ctx, socketPath, *interval); err != nil {
		return fmt.Errorf("wait health via %s: %w", socketPath, err)
	}
	return nil
}

func runGetTask(args []string) error {
	fs := flag.NewFlagSet("get-task", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socketPath := fs.String("socket-path", client.DefaultSocketPath(), "UNIX socket path")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageError("get-task requires <task-id>")
	}

	task, err := getTask(*socketPath, fs.Arg(0))
	if err != nil {
		return err
	}
	return printJSON(task)
}

func runWaitTaskStatus(args []string) error {
	fs := flag.NewFlagSet("wait-task-status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	timeout := fs.Duration("timeout", 15*time.Second, "maximum wait time")
	interval := fs.Duration("interval", 100*time.Millisecond, "poll interval")
	socketPath := fs.String("socket-path", client.DefaultSocketPath(), "UNIX socket path")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return usageError("wait-task-status requires <task-id> <status>")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	task, err := waitTaskStatus(ctx, *socketPath, fs.Arg(0), fs.Arg(1), *interval)
	if err != nil {
		return err
	}
	return printJSON(task)
}

func runListJobs(args []string) error {
	fs := flag.NewFlagSet("list-jobs", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socketPath := fs.String("socket-path", client.DefaultSocketPath(), "UNIX socket path")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageError("list-jobs requires <task-id>")
	}

	jobs, err := listJobs(*socketPath, fs.Arg(0))
	if err != nil {
		return err
	}
	return printJSON(jobs)
}

func runWaitJobCount(args []string) error {
	fs := flag.NewFlagSet("wait-job-count", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	timeout := fs.Duration("timeout", 15*time.Second, "maximum wait time")
	interval := fs.Duration("interval", 100*time.Millisecond, "poll interval")
	socketPath := fs.String("socket-path", client.DefaultSocketPath(), "UNIX socket path")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 2 {
		return usageError("wait-job-count requires <task-id> <count>")
	}

	wantCount, err := strconv.Atoi(fs.Arg(1))
	if err != nil || wantCount < 0 {
		return usageError("wait-job-count requires <task-id> <count>")
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	jobs, err := waitJobCount(ctx, *socketPath, fs.Arg(0), wantCount, *interval)
	if err != nil {
		return err
	}
	return printJSON(jobs)
}

func runAssertJobRoleCount(args []string) error {
	fs := flag.NewFlagSet("assert-job-role-count", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	socketPath := fs.String("socket-path", client.DefaultSocketPath(), "UNIX socket path")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 3 {
		return usageError("assert-job-role-count requires <task-id> <role> <count>")
	}

	wantCount, err := strconv.Atoi(fs.Arg(2))
	if err != nil || wantCount < 0 {
		return usageError("assert-job-role-count requires <task-id> <role> <count>")
	}

	jobs, err := listJobs(*socketPath, fs.Arg(0))
	if err != nil {
		return err
	}
	gotCount := countJobsByRole(jobs, fs.Arg(1))
	if gotCount != wantCount {
		return fmt.Errorf("job role count mismatch for %q: got %d, want %d", fs.Arg(1), gotCount, wantCount)
	}
	return printJSON(jobs)
}

func resolveSocketPath(args []string) string {
	if len(args) > 0 && args[0] != "" {
		return args[0]
	}
	return client.DefaultSocketPath()
}

func waitUnixSocket(ctx context.Context, socketPath string, interval time.Duration) error {
	for {
		conn, err := net.DialTimeout("unix", socketPath, interval)
		if err == nil {
			_ = conn.Close()
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func waitHealth(ctx context.Context, socketPath string, interval time.Duration) error {
	c := client.NewUnixClient(socketPath)

	for {
		var resp struct {
			Status string `json:"status"`
		}
		err := c.Do("GET", "/api/health", nil, &resp)
		if err == nil && resp.Status == "ok" {
			return nil
		}

		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("%w: last error: %v", ctx.Err(), err)
			}
			return fmt.Errorf("%w: unexpected health status %q", ctx.Err(), resp.Status)
		case <-time.After(interval):
		}
	}
}

func getTask(socketPath, taskID string) (*orchestrator.Task, error) {
	c := client.NewUnixClient(socketPath)
	var task orchestrator.Task
	if err := c.Do("GET", "/api/tasks/"+taskID, nil, &task); err != nil {
		return nil, fmt.Errorf("get task %s: %w", taskID, err)
	}
	return &task, nil
}

func waitTaskStatus(ctx context.Context, socketPath, taskID, wantStatus string, interval time.Duration) (*orchestrator.Task, error) {
	for {
		task, err := getTask(socketPath, taskID)
		if err == nil && string(task.Status) == wantStatus {
			return task, nil
		}

		select {
		case <-ctx.Done():
			if err != nil {
				return nil, fmt.Errorf("%w: last error: %v", ctx.Err(), err)
			}
			return nil, fmt.Errorf("%w: task %s did not reach status %s", ctx.Err(), taskID, wantStatus)
		case <-time.After(interval):
		}
	}
}

func listJobs(socketPath, taskID string) ([]map[string]any, error) {
	c := client.NewUnixClient(socketPath)
	var jobs []map[string]any
	if err := c.Do("GET", "/api/jobs?task_id="+taskID, nil, &jobs); err != nil {
		return nil, fmt.Errorf("list jobs for task %s: %w", taskID, err)
	}
	return jobs, nil
}

func waitJobCount(ctx context.Context, socketPath, taskID string, wantCount int, interval time.Duration) ([]map[string]any, error) {
	for {
		jobs, err := listJobs(socketPath, taskID)
		if err == nil && len(jobs) >= wantCount {
			return jobs, nil
		}

		select {
		case <-ctx.Done():
			if err != nil {
				return nil, fmt.Errorf("%w: last error: %v", ctx.Err(), err)
			}
			return nil, fmt.Errorf("%w: task %s did not reach job count %d", ctx.Err(), taskID, wantCount)
		case <-time.After(interval):
		}
	}
}

func countJobsByRole(jobs []map[string]any, role string) int {
	count := 0
	for _, job := range jobs {
		if fmt.Sprint(job["role"]) == role {
			count++
		}
	}
	return count
}

func runWSJobOutput(args []string) error {
	fs := flag.NewFlagSet("ws-job-output", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	addr := fs.String("addr", "", "HTTP base address (e.g. http://127.0.0.1:8080)")
	jobID := fs.String("job", "", "job ID to attach to")
	timeout := fs.Duration("timeout", 10*time.Second, "maximum wait time")
	contains := fs.String("contains", "", "expected string that must appear in accumulated output")
	cookie := fs.String("cookie", "", "optional Cookie header value (e.g. boid_session=VALUE)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *addr == "" || *jobID == "" {
		return usageError("ws-job-output requires --addr and --job")
	}

	wsURL := strings.Replace(*addr, "http://", "ws://", 1)
	wsURL += "/api/jobs/" + *jobID + "/attach/ws"

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	dialOpts := &websocket.DialOptions{}
	if *cookie != "" {
		dialOpts.HTTPHeader = map[string][]string{"Cookie": {*cookie}}
	}

	conn, _, err := websocket.Dial(ctx, wsURL, dialOpts)
	if err != nil {
		return fmt.Errorf("dial %s: %w", wsURL, err)
	}
	defer conn.CloseNow()

	var buf []byte
	for {
		_, raw, err := conn.Read(ctx)
		if err != nil {
			if *contains != "" && strings.Contains(string(buf), *contains) {
				return nil
			}
			return fmt.Errorf("websocket read: %w (accumulated output: %q)", err, buf)
		}

		var msg struct {
			Type string `json:"type"`
			Data string `json:"data,omitempty"`
			Code int    `json:"code,omitempty"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "output":
			decoded, decErr := base64.StdEncoding.DecodeString(msg.Data)
			if decErr == nil {
				buf = append(buf, decoded...)
				fmt.Fprintf(os.Stdout, "[ws-output] %s", decoded)
				if *contains != "" && strings.Contains(string(buf), *contains) {
					conn.Close(websocket.StatusNormalClosure, "done")
					return nil
				}
			}
		case "exit":
			if *contains != "" {
				if strings.Contains(string(buf), *contains) {
					return nil
				}
				return fmt.Errorf("job exited (code %d) without expected output %q (got: %q)", msg.Code, *contains, buf)
			}
			return nil
		case "error":
			return fmt.Errorf("server error: %s", raw)
		}
	}
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(v)
}

func usageError(msg string) error {
	return errors.New(msg + "\nusage: boid-e2e <wait-unix-socket|wait-health|get-task|wait-task-status|list-jobs|wait-job-count|assert-job-role-count|ws-job-output|fake-docker|upstream-serve> ...")
}

// runUpstreamServe starts a fixture upstream git-over-HTTPS server
// (e2e/upstream) for the e2e harness (docs/plans/git-gateway-cutover.md
// PR7a). It pre-creates a bare repo for every name given as a positional
// argument, writes the self-signed TLS certificate it serves with to
// --cert-file (if set — see upstream.New's doc comment for why the fixture
// serves real TLS at all) and the bound "host:port" to --ready-file (if
// set), then blocks until SIGINT/SIGTERM.
//
// --cert-file is written before --ready-file so the calling shell — which
// synchronizes on the ready file via e2e_wait_for_file — is guaranteed to
// see the cert file already in place (e2e/lib/common.sh reads it right
// after the ready-file wait returns, to configure SSL_CERT_FILE before
// starting the boid daemon).
//
// Usage: boid-e2e upstream-serve --dir <path> [--addr host:port]
//
//	[--ready-file <path>] [--cert-file <path>] [--git-bin <path>] <repo-name>...
func runUpstreamServe(args []string) error {
	fs := flag.NewFlagSet("upstream-serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", "", "parent directory for bare repositories (required)")
	addr := fs.String("addr", "127.0.0.1:0", "listen address")
	readyFile := fs.String("ready-file", "", "file to write the bound host:port to once listening")
	certFile := fs.String("cert-file", "", "file to write the server's self-signed TLS certificate (PEM) to")
	gitBin := fs.String("git-bin", "", "path to the real git binary (default /usr/bin/git)")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return usageError("upstream-serve requires --dir")
	}

	u, err := upstream.New(upstream.Options{Dir: *dir, Addr: *addr, GitBin: *gitBin})
	if err != nil {
		return fmt.Errorf("upstream-serve: %w", err)
	}
	defer u.Close()

	for _, name := range fs.Args() {
		if _, err := u.NewRepo(name); err != nil {
			return fmt.Errorf("upstream-serve: create repo %q: %w", name, err)
		}
	}

	if *certFile != "" {
		if err := os.WriteFile(*certFile, u.CertPEM(), 0o644); err != nil {
			return fmt.Errorf("upstream-serve: write cert file: %w", err)
		}
	}
	if *readyFile != "" {
		if err := os.WriteFile(*readyFile, []byte(u.Addr()), 0o644); err != nil {
			return fmt.Errorf("upstream-serve: write ready file: %w", err)
		}
	}
	fmt.Fprintf(os.Stderr, "[upstream] listening on %s (dir=%s)\n", u.Addr(), *dir)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	<-ctx.Done()
	return nil
}

// runFakeDocker starts a minimal Docker API-compatible HTTP server on a Unix
// socket.  It returns fixed responses for Docker create/stop/delete endpoints
// and logs every request to a JSONL file (one entry per line).  The server
// runs until it receives SIGTERM or SIGINT.
//
// Usage: boid-e2e fake-docker [--log <logfile>] <socket-path>
func runFakeDocker(args []string) error {
	fs := flag.NewFlagSet("fake-docker", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	logPath := fs.String("log", "", "path to request log file (JSONL, optional)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageError("fake-docker requires <socket-path>")
	}
	socketPath := fs.Arg(0)

	// Remove stale socket file if present.
	_ = os.Remove(socketPath)

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("fake-docker listen %s: %w", socketPath, err)
	}
	defer ln.Close()

	// Open log file (append+create so multiple runs accumulate).
	var (
		logMu  sync.Mutex
		logF   *os.File
	)
	if *logPath != "" {
		logF, err = os.OpenFile(*logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return fmt.Errorf("fake-docker open log: %w", err)
		}
		defer logF.Close()
	}

	writeLog := func(method, path string, status int) {
		if logF == nil {
			return
		}
		line := fmt.Sprintf("{\"method\":%q,\"path\":%q,\"status\":%d}\n", method, path, status)
		logMu.Lock()
		defer logMu.Unlock()
		_, _ = logF.WriteString(line)
		_ = logF.Sync()
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.ToUpper(r.Method)
		path := r.URL.Path
		status, body := fakeDockerRoute(method, path)
		writeLog(method, path, status)
		if len(body) > 0 {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		if len(body) > 0 {
			_, _ = w.Write(body)
		}
	})

	srv := &http.Server{Handler: handler}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()

	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("fake-docker serve: %w", err)
	}
	return nil
}

// fakeDockerRoute returns the (statusCode, body) for a fake Docker API
// response.  It covers the endpoints exercised by the docker proxy E2E tests.
func fakeDockerRoute(method, path string) (int, []byte) {
	switch {
	case method == "GET" || method == "HEAD":
		return http.StatusOK, []byte(`{}`)
	case method == "POST" && path == "/containers/create":
		return http.StatusCreated, []byte(`{"Id":"fake-c1","Warnings":[]}`)
	case method == "POST" && path == "/networks/create":
		return http.StatusCreated, []byte(`{"Id":"fake-n1","Warning":""}`)
	case method == "POST" && path == "/volumes/create":
		return http.StatusCreated, []byte(`{"Name":"fake-v1","Driver":"local","Mountpoint":"","Labels":{},"Scope":"local"}`)
	case method == "DELETE":
		return http.StatusNoContent, nil
	case method == "POST":
		return http.StatusNoContent, nil
	default:
		return http.StatusOK, []byte(`{}`)
	}
}
