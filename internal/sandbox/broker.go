package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type tokenEntry struct {
	Context         TokenContext
	Commands        map[string]CommandDef
	BuiltinPolicies map[string]BuiltinPolicy
}

type Broker struct {
	SocketPath      string
	BoidBinary      string
	BoidExecutor    BoidExecutor
	ProjectResolver ProjectResolver
	listener        net.Listener
	mu              sync.RWMutex
	registry        map[string]*tokenEntry
	// lifecycleCtx is the context passed to Start. It parents every per-request
	// context so a daemon shutdown cancels in-flight blocking ops (task_ask).
	// nil when Start has not been called (e.g. tests that drive Handle directly),
	// in which case baseContext falls back to context.Background().
	lifecycleCtx context.Context
}

// baseContext returns the parent context for a request: the broker's lifecycle
// context when running under Start, otherwise a background context.
func (b *Broker) baseContext() context.Context {
	if b.lifecycleCtx != nil {
		return b.lifecycleCtx
	}
	return context.Background()
}

// resolveProjectRef applies the broker's ProjectResolver when configured.
// Empty refs and nil resolver both short-circuit to the input so callers
// don't need to special-case either.
func (b *Broker) resolveProjectRef(ref string) (string, error) {
	if b.ProjectResolver == nil || ref == "" {
		return ref, nil
	}
	return b.ProjectResolver(ref)
}

func (b *Broker) Register(commands map[string]CommandDef, builtinPolicies map[string]BuiltinPolicy, ctx TokenContext) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.registry == nil {
		b.registry = make(map[string]*tokenEntry)
	}

	token := generateToken()
	entry := &tokenEntry{
		Context:         ctx,
		Commands:        commands,
		BuiltinPolicies: builtinPolicies,
	}
	b.registry[token] = entry
	return token
}

type SecretResolver func(key string) (string, error)

func (b *Broker) RegisterWithSecrets(commands map[string]CommandDef, builtinPolicies map[string]BuiltinPolicy, ctx TokenContext, resolver SecretResolver) string {
	resolved := make(map[string]CommandDef, len(commands))
	for name, def := range commands {
		if len(def.Env) > 0 {
			newEnv := make(map[string]string, len(def.Env))
			var missing []string
			for k, v := range def.Env {
				if strings.HasPrefix(v, "secret:") {
					secretKey := v[len("secret:"):]
					if secretKey == "" {
						secretKey = k // env var name as secret key
					}
					val, err := resolver(secretKey)
					if err != nil {
						slog.Warn("failed to resolve secret; host_command will be rejected at exec time",
							"command", def.Name, "env", k, "key", secretKey, "error", err)
						missing = append(missing, fmt.Sprintf("%s (secret:%s)", k, secretKey))
						continue
					}
					newEnv[k] = val
				} else {
					newEnv[k] = v
				}
			}
			def.Env = newEnv
			if len(missing) > 0 {
				def.MissingSecrets = append(def.MissingSecrets, missing...)
			}
		}
		resolved[name] = def
	}
	return b.Register(resolved, builtinPolicies, ctx)
}

func (b *Broker) GetContext(token string) (TokenContext, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	entry, ok := b.registry[token]
	if !ok {
		return TokenContext{}, false
	}
	return entry.Context, true
}

func (b *Broker) Unregister(token string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.registry, token)
}

func (b *Broker) Start(ctx context.Context) error {
	os.Remove(b.SocketPath)
	ln, err := net.Listen("unix", b.SocketPath)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	b.listener = ln
	// Stored before the accept goroutine starts, so per-connection handlers
	// (which only run after Accept) observe it without a data race.
	b.lifecycleCtx = ctx

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go b.handleConn(conn)
		}
	}()

	go func() {
		<-ctx.Done()
		b.Stop()
	}()

	return nil
}

func (b *Broker) Stop() {
	if b.listener != nil {
		b.listener.Close()
	}
	os.Remove(b.SocketPath)
}

func (b *Broker) handleConn(conn net.Conn) {
	defer conn.Close()

	var req ExecRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		return
	}

	if req.Streaming {
		b.handleStreamingExec(conn, &req)
		return
	}

	ctx := b.baseContext()
	// A blocking ask holds this connection open until an answer arrives. Tie a
	// per-connection context to the socket so that if the sandbox dies (or the
	// daemon shuts down) the server-side wait unblocks instead of leaking.
	if isBlockingAskRequest(&req) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		go watchConnClose(conn, cancel)
	}

	resp := b.handle(ctx, &req)
	_ = json.NewEncoder(conn).Encode(resp) // best-effort; peer may have hung up
}

// isBlockingAskRequest reports whether req is a `boid task ask` builtin call,
// which the broker must handle by holding the connection open.
func isBlockingAskRequest(req *ExecRequest) bool {
	return req.Boid != nil && req.Boid.Op == BoidOpTaskAsk
}

// watchConnClose cancels the connection context when the peer closes the socket.
// It only cancels on a read error (EOF / reset); stray bytes (e.g. a trailing
// newline left by the request encoder) are ignored so a still-open connection is
// never treated as closed. It exits when the read fails, which also happens when
// handleConn's deferred Close runs after a normal response.
func watchConnClose(conn net.Conn, cancel context.CancelFunc) {
	buf := make([]byte, 64)
	for {
		if _, err := conn.Read(buf); err != nil {
			cancel()
			return
		}
	}
}

// sendStreamResponse converts a completed ExecResponse to the streaming chunk
// format. Used when a boid/git builtin is called with Streaming=true, or as
// a fallback on platforms where PTY-based streaming is unavailable.
func sendStreamResponse(conn net.Conn, resp *ExecResponse) {
	enc := json.NewEncoder(conn)
	if resp.Stdout != "" {
		_ = enc.Encode(&StreamChunk{Type: StreamTypeStdout, Data: resp.Stdout})
	}
	if resp.Stderr != "" {
		_ = enc.Encode(&StreamChunk{Type: StreamTypeStderr, Data: resp.Stderr})
	}
	_ = enc.Encode(&StreamChunk{Type: StreamTypeExit, ExitCode: resp.ExitCode})
}

// Handle dispatches a request using the broker's base context. Retained for
// callers (mainly tests) that drive the broker synchronously without a
// per-connection context; the live socket path uses handle with a request ctx.
func (b *Broker) Handle(req *ExecRequest) *ExecResponse {
	return b.handle(b.baseContext(), req)
}

func (b *Broker) handle(ctx context.Context, req *ExecRequest) *ExecResponse {
	b.mu.RLock()
	entry, ok := b.registry[req.Token]
	b.mu.RUnlock()

	if !ok {
		return &ExecResponse{ExitCode: 1, Stderr: "invalid token"}
	}

	// Boid builtin is identified by the typed payload, not by the binary path.
	// The shim only attaches req.Boid when the caller went through the boid
	// CLI shim entry point.
	if req.Boid != nil {
		return b.handleBoidBuiltin(ctx, req, entry)
	}

	// Fetch builtin: broker-side HTTP GET dispatched via boid shim.
	if req.Fetch != nil {
		return handleFetchBuiltin(req, entry)
	}

	// git is no longer a broker builtin (docs/plans/git-gateway-cutover.md
	// PR8): sandbox git is the real binary visible via the base rbind of
	// /usr, so a "git"-named command reaching the broker at all would only
	// happen via an explicit host_commands entry — same handling as any
	// other command name.
	def, ok := entry.Commands[req.Command]
	if !ok {
		return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("command not allowed: %s", filepath.Base(req.Command))}
	}

	return b.execCommand(req, def, entry)
}

func (b *Broker) handleBoidBuiltin(ctx context.Context, req *ExecRequest, entry *tokenEntry) *ExecResponse {
	// req.Boid is guaranteed non-nil — Handle dispatches here only when the
	// shim attaches a typed boid payload.
	if !entry.hasBuiltinPolicy("boid") {
		return &ExecResponse{ExitCode: 1, Stderr: "command not allowed: boid"}
	}

	if err := validateBoidBuiltinCwd(req.Cwd, entry); err != nil {
		return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
	}

	boidReq := *req.Boid
	if !entry.allowsBuiltinOp("boid", string(boidReq.Op)) {
		return &ExecResponse{
			ExitCode: 1,
			Stderr:   fmt.Sprintf("boid op %q not allowed by policy", boidReq.Op),
		}
	}

	switch boidReq.Op {
	case BoidOpJobDone:
		if boidReq.JobID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid job done requires a job id"}
		}
		if boidReq.JobID != entry.Context.JobID {
			return &ExecResponse{ExitCode: 1, Stderr: "boid job done is restricted to the current job"}
		}
	case BoidOpAgentStop:
		if boidReq.JobID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid agent stop requires a job id"}
		}
		if boidReq.JobID != entry.Context.JobID {
			return &ExecResponse{ExitCode: 1, Stderr: "boid agent stop is restricted to the current job"}
		}
	case BoidOpTaskCreate:
		if boidReq.ProjectID == "" {
			boidReq.ProjectID = entry.Context.ProjectID
		}
		resolved, err := b.resolveProjectRef(boidReq.ProjectID)
		if err != nil {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task create: resolve project %q: %s", boidReq.ProjectID, err)}
		}
		boidReq.ProjectID = resolved
		if !entry.Context.AllowsProject(boidReq.ProjectID) {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task create is restricted to the current workspace"}
		}
	case BoidOpTaskGet:
		if boidReq.TaskID == "" {
			boidReq.TaskID = entry.Context.TaskID
		}
	case BoidOpTaskAsk:
		// `boid task ask` targets the caller's own task; the shim leaves TaskID
		// empty, so fill it from the token context (project authorization runs
		// in the executor, as for task_get / task_notify).
		if boidReq.TaskID == "" {
			boidReq.TaskID = entry.Context.TaskID
		}
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task ask requires a task id"}
		}
		if boidReq.Question == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task ask requires a question"}
		}
	case BoidOpTaskUpdate:
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task update requires a task id"}
		}
		// 更新対象 task の project_id 検証は boid_executor 側で行う
		// (broker は TaskStore を持たないため、ここでは ID の有無のみチェック)
	case BoidOpTaskReopen:
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task reopen requires a task id"}
		}
	case BoidOpTaskNotify:
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task notify requires a task id"}
		}
		if boidReq.Message == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task notify requires a message"}
		}
		// project 検証は boid_executor 側で行う (TaskStore 経由で task の project_id を引く)
	case BoidOpTaskAnswer:
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task answer requires a task id"}
		}
		if boidReq.QuestionID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task answer requires a question id"}
		}
		if boidReq.Answer == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task answer requires an answer"}
		}
		// project 検証は boid_executor 側で行う
	case BoidOpTaskList:
		// project_id 指定があれば解決して AllowsProject 検査
		if boidReq.ProjectID != "" {
			resolved, err := b.resolveProjectRef(boidReq.ProjectID)
			if err != nil {
				return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task list: resolve project %q: %s", boidReq.ProjectID, err)}
			}
			boidReq.ProjectID = resolved
			if !entry.Context.AllowsProject(boidReq.ProjectID) {
				return &ExecResponse{ExitCode: 1, Stderr: "boid task list: project is outside the current workspace"}
			}
		}
		// workspace_id 指定があれば context と一致確認 (escape hatch なし)
		if boidReq.WorkspaceID != "" {
			if boidReq.WorkspaceID != entry.Context.WorkspaceID {
				return &ExecResponse{ExitCode: 1, Stderr: "boid task list: workspace_id is outside the current workspace"}
			}
		}
		// 両方未指定: WorkspaceID が非空なら自動 inject、空なら executor が AllowedProjectIDs でフィルタ
		if boidReq.ProjectID == "" && boidReq.WorkspaceID == "" {
			if entry.Context.WorkspaceID != "" {
				boidReq.WorkspaceID = entry.Context.WorkspaceID
			}
		}
	case BoidOpActionSend:
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid action send requires a task id"}
		}
		if boidReq.ActionType == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid action send requires a type"}
		}
		// project 検証は boid_executor 側で行う (task_notify と同じパターン)
	case BoidOpJobList:
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid job list requires a task id"}
		}
		// project 検証は boid_executor 側で行う
	case BoidOpJobShow:
		if boidReq.JobID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid job show requires a job id"}
		}
	case BoidOpJobLog:
		if boidReq.JobID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid job log requires a job id"}
		}
	case BoidOpTaskImport:
		if len(boidReq.ImportTasks) == 0 {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task import requires at least one task"}
		}
		// Override を先に resolve してから per-task 検証に入ると、下流で
		// 再解決するか否かを per-task 分岐に持ち込まなくて済む。
		if boidReq.ImportProjectOverride != "" {
			overridden, err := b.resolveProjectRef(boidReq.ImportProjectOverride)
			if err != nil {
				return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task import: resolve project %q: %s", boidReq.ImportProjectOverride, err)}
			}
			boidReq.ImportProjectOverride = overridden
		}
		// ImportTasks は req.Boid と配列を共有しているため、mutate 前に
		// slice header を複製して caller 側の BoidRequest に影響しないよう隔離する。
		if boidReq.ImportProjectOverride == "" && b.ProjectResolver != nil {
			tasks := make([]json.RawMessage, len(boidReq.ImportTasks))
			copy(tasks, boidReq.ImportTasks)
			boidReq.ImportTasks = tasks
		}
		// バッチ全体の project_id 事前検証 (名前解決も同時に行う)
		for i, raw := range boidReq.ImportTasks {
			var peek struct {
				ProjectID string `json:"project_id"`
			}
			if err := json.Unmarshal(raw, &peek); err != nil {
				return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task import: line %d: invalid task json: %s", i+1, err)}
			}
			projectID := peek.ProjectID
			if boidReq.ImportProjectOverride != "" {
				projectID = boidReq.ImportProjectOverride
			} else if peek.ProjectID != "" {
				resolved, err := b.resolveProjectRef(peek.ProjectID)
				if err != nil {
					return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task import: line %d: resolve project %q: %s", i+1, peek.ProjectID, err)}
				}
				if resolved != peek.ProjectID {
					updated, err := rewriteImportTaskProjectID(raw, resolved)
					if err != nil {
						return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task import: line %d: rewrite project_id: %s", i+1, err)}
					}
					boidReq.ImportTasks[i] = updated
				}
				projectID = resolved
			}
			if projectID == "" {
				projectID = entry.Context.ProjectID
			}
			if !entry.Context.AllowsProject(projectID) {
				return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task import: line %d: project %q is outside the current workspace", i+1, projectID)}
			}
		}
	}

	if b.BoidExecutor == nil {
		return &ExecResponse{ExitCode: 1, Stderr: "boid builtin unavailable"}
	}
	return b.BoidExecutor.ExecuteBoidBuiltin(ctx, entry.Context, &boidReq)
}

// rewriteImportTaskProjectID replaces the "project_id" field of a task import
// raw JSON with newID, preserving all other fields. Decode → mutate → encode
// via map[string]json.RawMessage keeps unknown fields intact without requiring
// a schema update here.
func rewriteImportTaskProjectID(raw json.RawMessage, newID string) (json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(newID)
	if err != nil {
		return nil, err
	}
	if fields == nil {
		fields = make(map[string]json.RawMessage, 1)
	}
	fields["project_id"] = encoded
	return json.Marshal(fields)
}

func validateBoidBuiltinCwd(cwd string, entry *tokenEntry) error {
	if cwd == "" {
		return fmt.Errorf("cwd required")
	}
	if !filepath.IsAbs(cwd) {
		return fmt.Errorf("cwd must be absolute")
	}

	// Clone-mode jobs (docs/plans/git-gateway-cutover.md PR6 cutover) declare
	// cwd as a sandbox-internal, name-scoped subdirectory of "/workspace"
	// (dispatcher.sandboxCloneDir — workspace 親化リファクタリング,
	// nose 2026-07-13 decision) — entryRoot already special-cases this via
	// entry.Context.SandboxRoot (see its own doc comment: "clone-mode jobs
	// have no host-side ProjectDir the sandbox's own filesystem corresponds
	// to"). The broker itself always runs on the host, outside
	// any sandbox mount namespace, so os.Stat(cwd) below can never see that
	// path — it would either ENOENT ("cwd does not exist" on a host with no
	// coincidental directory of that name) or, worse, silently validate
	// against an unrelated host directory that happens to share the name.
	// Skip the filesystem check entirely for clone-mode entries and fall through to
	// the same path-membership validation every other cwd already goes
	// through (entryRoot / isWithinRoot below) — this is exactly what let
	// every clone-mode hook's `boid job done` (postJobDone) silently fail
	// validation and get swallowed as a non-fatal error, which the daemon's
	// "runtime exited without boid job done" fallback then mistook for a
	// crash rather than the hook's real (successful) exit code.
	if entry == nil || entry.Context.SandboxRoot == "" {
		info, err := os.Stat(cwd)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("cwd does not exist")
			}
			return fmt.Errorf("stat cwd: %w", err)
		}
		if !info.IsDir() {
			return fmt.Errorf("cwd must be a directory")
		}
	}

	if entry != nil {
		if policy, ok := entry.BuiltinPolicies["boid"]; ok && policy.AllowsCwd(cwd) {
			return nil
		}
	}

	if root := entryRoot(entry); root != "" && isWithinRoot(cwd, root) {
		return nil
	}
	return fmt.Errorf("boid builtin is restricted to the current project or worktree")
}

// entryRoot returns the directory a "boid" builtin call's cwd argument must
// fall under. Clone-mode jobs (docs/plans/git-gateway-cutover.md PR6 cutover)
// have no host-side ProjectDir the sandbox's own filesystem corresponds to —
// their cwd is always a name-scoped subdirectory of the sandbox-internal
// "/workspace" (workspace 親化リファクタリング, nose 2026-07-13 decision) —
// so SandboxRoot takes priority when set.
func entryRoot(entry *tokenEntry) string {
	if entry == nil {
		return ""
	}
	if entry.Context.SandboxRoot != "" {
		return entry.Context.SandboxRoot
	}
	return entry.Context.ProjectDir
}

func isWithinRoot(path, root string) bool {
	if path == root {
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

func (b *Broker) execCommand(req *ExecRequest, def CommandDef, entry *tokenEntry) *ExecResponse {
	if msg, ok := gateHostCommand(def, req.Args); !ok {
		return &ExecResponse{ExitCode: 1, Stderr: msg}
	}

	binary := def.Path
	if binary == "" {
		resolved, err := exec.LookPath(def.Name)
		if err != nil {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("host_commands.%s: unable to locate %q in PATH: %v", def.Name, def.Name, err)}
		}
		binary = resolved
	}
	cmd := exec.Command(binary, req.Args...)
	cmd.Dir = hostCommandCwd()

	cmd.Env = hostCommandEnv(def.Env)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
	}

	return &ExecResponse{ExitCode: exitCode, Stdout: stdout.String(), Stderr: stderr.String()}
}

// hostCommandEnv builds the environment passed to a host_command child
// process. It inherits the broker's environment minus BOID_* internal markers
// (notably BOID_DAEMON_CHILD, which would otherwise re-enter daemon-child
// mode in any boid CLI invoked by the host_command, and BOID_BROKER_SOCKET /
// BOID_BROKER_TOKEN, which would let the child speak to the broker as if it
// were a sandbox process). defEnv overlays the inherited values when set.
func hostCommandEnv(defEnv map[string]string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(defEnv))
	for _, kv := range base {
		if strings.HasPrefix(kv, "BOID_") {
			continue
		}
		out = append(out, kv)
	}
	for k, v := range defEnv {
		out = append(out, k+"="+v)
	}
	return out
}

// hostCommandCwd returns the working directory for a host command process.
//
// Contract: host commands must not depend on a repo checkout being present
// on the host side. Neither the sandbox-side cwd (req.Cwd) nor the token's
// host-side context (ProjectDir) is consulted here — container
// backends have no host checkout at all, so any repo context a host command
// needs must come from ${boid:repo_slug} env expansion at token-registration
// time (see dispatcher.ResolveHostCommands), not from cwd. A neutral,
// always-present directory keeps host commands portable across runtimes.
func hostCommandCwd() string {
	return os.TempDir()
}

func (e *tokenEntry) hasBuiltinPolicy(name string) bool {
	if e == nil || len(e.BuiltinPolicies) == 0 {
		return false
	}
	_, ok := e.BuiltinPolicies[name]
	return ok
}

func (e *tokenEntry) allowsBuiltinOp(name, op string) bool {
	if e == nil || len(e.BuiltinPolicies) == 0 {
		return false
	}
	policy, ok := e.BuiltinPolicies[name]
	if !ok {
		return false
	}
	return policy.Allows(op)
}

// gateHostCommand runs the common pre-exec policy checks shared by the
// non-streaming and streaming host-command paths: reject rules, missing
// declared secrets, and the allow/deny argument policy. Reject rules are
// checked first so a matching invocation gets the actionable "rejected:
// <reason>" message instead of the generic "arguments not allowed" one.
// Returns (stderr, true) when the call is allowed to proceed — stderr is then
// meaningless and should be ignored by the caller — or (stderr, false) with
// the message to surface when the call is blocked.
func gateHostCommand(def CommandDef, args []string) (string, bool) {
	joined := strings.Join(args, " ")
	for _, rule := range def.RejectRules {
		if globMatch(rule.Match, joined) {
			return fmt.Sprintf("host_commands.%s: rejected: %s", def.Name, rule.Reason), false
		}
	}

	if msg := def.MissingSecretsMessage(); msg != "" {
		return msg, false
	}

	if !CheckPolicy(def, args) {
		return "arguments not allowed", false
	}

	return "", true
}

func generateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("generateToken: crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b)
}
