package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
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

	// TLSAddr, when non-empty, additionally binds a TCP+mTLS listener at
	// this address (docs/plans/phase6-container-backend.md §PR4/§決定5:
	// "gateway / broker / dockerproxy はサービス名 (DNS) + TCP (mTLS) で到達す
	// る"). This is purely additive — the UNIX socket above (SocketPath)
	// is bound exactly as before regardless of TLSAddr, so the userns
	// backend's sandbox-side broker RPC client (which still dials
	// SocketPath) is unaffected. TLSConfig must be non-nil whenever
	// TLSAddr is set (see internal/mtls.CA.ServerTLSConfig for how to
	// build one with mutual-TLS auth wired up); a caller that leaves
	// TLSAddr empty (the zero value, matched by every existing caller and
	// test today) gets the pre-PR4 UNIX-only behavior unchanged.
	//
	// internal/server.Server sets these (when its Config.TLSDir is
	// configured) so a real daemon actually binds this listener
	// alongside the UNIX socket. No client dials it yet in PR4 — the
	// container backend that will is PR5 — so today it is a live but
	// unconsumed listener, exercised directly by
	// TestBrokerTCPListener_MutualTLSHandshake.
	TLSAddr   string
	TLSConfig *tls.Config

	tlsListener net.Listener
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
	// Stored before the accept goroutines start, so per-connection handlers
	// (which only run after Accept) observe it without a data race.
	b.lifecycleCtx = ctx

	go b.acceptLoop(ln)

	// TCP(mTLS) listener: additive alongside the UNIX socket above (see
	// the TLSAddr field doc comment). Both transports share the exact
	// same handleConn/handle dispatch chain — only the transport differs,
	// the ExecRequest/ExecResponse protocol does not (§決定5: "UNIX
	// socket を mTLS gRPC/HTTP に差し替えるだけで...意味論は無傷").
	if b.TLSAddr != "" {
		if b.TLSConfig == nil {
			ln.Close()
			return fmt.Errorf("broker: TLSAddr set without TLSConfig")
		}
		tln, err := tls.Listen("tcp", b.TLSAddr, b.TLSConfig)
		if err != nil {
			ln.Close()
			return fmt.Errorf("listen tls: %w", err)
		}
		b.tlsListener = tln
		go b.acceptLoop(tln)
	}

	go func() {
		<-ctx.Done()
		b.Stop()
	}()

	return nil
}

// acceptLoop accepts connections on ln until it is closed, dispatching
// each to handleConn on its own goroutine. Shared by both the UNIX socket
// and (when configured) the TCP+mTLS listener started from Start.
func (b *Broker) acceptLoop(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go b.handleConn(conn)
	}
}

// TLSListenAddr returns the bound address of the TCP+mTLS listener
// (e.g. "127.0.0.1:54321"), or "" if TLSAddr was empty or Start has not
// bound it yet.
func (b *Broker) TLSListenAddr() string {
	if b.tlsListener != nil {
		return b.tlsListener.Addr().String()
	}
	return ""
}

func (b *Broker) Stop() {
	if b.listener != nil {
		b.listener.Close()
	}
	if b.tlsListener != nil {
		b.tlsListener.Close()
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
	def, ok := lookupCommand(entry.Commands, req.Command)
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
	case BoidOpTaskWait:
		// Unlike task_ask, an empty TaskID is NOT filled in from the token
		// context: waiting on your own task deadlocks (you are the reason it
		// has not finished), so an omitted id is a mistake worth naming rather
		// than a default worth supplying. Project authorization runs in the
		// executor, as for every other task op here.
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task wait requires a task id"}
		}
	case BoidOpTaskNotify:
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task notify requires a task id"}
		}
		// --progress carries its own text and is delivered without a message:
		// NotifyTask's progress branch writes a timeline Action whose payload is
		// just the progress string and returns before touching message at all
		// (internal/api/task_notify.go). Its own gate is `message == "" &&
		// progress == ""`, and the shim likewise parses --progress without
		// requiring --message — this broker check had drifted to the stricter
		// rule, so the form boid's own skill doc teaches (`boid task notify
		// "$BOID_TASK_ID" --progress "<note>"`, internal/skills/data/boid-task/
		// SKILL.md) failed with exit=1 for every sandboxed caller.
		if boidReq.Message == "" && boidReq.Progress == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task notify requires a message or --progress"}
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
	case BoidOpProjectBehaviors:
		if boidReq.ProjectID == "" {
			boidReq.ProjectID = entry.Context.ProjectID
		}
		resolved, err := b.resolveProjectRef(boidReq.ProjectID)
		if err != nil {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid project behaviors: resolve project %q: %s", boidReq.ProjectID, err)}
		}
		boidReq.ProjectID = resolved
		if !entry.Context.AllowsProject(boidReq.ProjectID) {
			return &ExecResponse{ExitCode: 1, Stderr: "boid project behaviors: project is outside the current workspace"}
		}
	case BoidOpProjectList:
		// JobID-scoped like BoidOpTaskInstructions/Env/Payload, but
		// asymmetrically: `parseBoidProjectList` never gives a caller a way
		// to set JobID (no CLI flag), so a legitimate request always arrives
		// empty and gets defaulted here — unlike those ops, an empty JobID
		// is NOT an error. A caller-supplied JobID that doesn't match the
		// token's own is still rejected: BoidOpProjectList's response can
		// embed a peer's git gateway clone URL, which carries the target
		// job's own gateway token (e.g. PermFetchPush for a writable self
		// project) — without this check, a readonly job could name a
		// writable job's JobID and read its push-capable token back out
		// through the response, a readonly-to-writable escalation. Same
		// defense-in-depth rationale as BoidOpTaskInstructions/Env/Payload's
		// own equality check (a shim never emits a cross-job id, but a
		// handwritten request bypassing the shim could).
		if boidReq.JobID == "" {
			boidReq.JobID = entry.Context.JobID
		}
		if boidReq.JobID != entry.Context.JobID {
			return &ExecResponse{ExitCode: 1, Stderr: "boid project list is restricted to the current job"}
		}
	case BoidOpActionSend:
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid action send requires a task id"}
		}
		if boidReq.ActionType == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid action send requires a type"}
		}
		// project 検証は boid_executor 側で行う (task_notify と同じパターン)
	case BoidOpCardGet:
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid card get requires a task id"}
		}
		// project 検証は boid_executor 側で行う (action_send と同じパターン)
	case BoidOpCardList:
		// BoidOpTaskList と**同一の**スコーピングをここで行う (codex/Opus
		// レビュー High): task_list の scoping は executor ではなく broker 側に
		// あり、 project ref の解決 (name → UUID) もここでしか行われない。
		// 「executor 側で AllowedProjectIDs で見る」だけでは
		// (a) --workspace-id が無検査で通り他 workspace の card が
		// 丸ごと読める (ListTasks の WorkspaceID filter は project_workspaces を
		// INNER JOIN する = 本当に workspace を跨ぐ)、
		// (b) project 名を渡すと UUID 空間の AllowsProject と突き合わされて
		// 常に失敗する、 の 2 つが起きる。
		if boidReq.ProjectID != "" {
			resolved, err := b.resolveProjectRef(boidReq.ProjectID)
			if err != nil {
				return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid card list: resolve project %q: %s", boidReq.ProjectID, err)}
			}
			boidReq.ProjectID = resolved
			if !entry.Context.AllowsProject(boidReq.ProjectID) {
				return &ExecResponse{ExitCode: 1, Stderr: "boid card list: project is outside the current workspace"}
			}
		}
		if boidReq.WorkspaceID != "" && boidReq.WorkspaceID != entry.Context.WorkspaceID {
			return &ExecResponse{ExitCode: 1, Stderr: "boid card list: workspace_id is outside the current workspace"}
		}
		if boidReq.ProjectID == "" && boidReq.WorkspaceID == "" && entry.Context.WorkspaceID != "" {
			boidReq.WorkspaceID = entry.Context.WorkspaceID
		}
	case BoidOpTaskIdentityLink:
		// docs/plans/ingestion-identity.md PR-1 (B-1): scoping is
		// broker-authoritative, matching BoidOpTaskCreate exactly (default
		// from ctx, resolve, AllowsProject) — see BoidOpTaskIdentityLink's
		// own doc comment in protocol.go for why this is not left to the
		// executor alone.
		if boidReq.Identity == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task identity link requires an identity"}
		}
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task identity link requires a task id"}
		}
		if boidReq.ProjectID == "" {
			boidReq.ProjectID = entry.Context.ProjectID
		}
		resolved, err := b.resolveProjectRef(boidReq.ProjectID)
		if err != nil {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task identity link: resolve project %q: %s", boidReq.ProjectID, err)}
		}
		boidReq.ProjectID = resolved
		if !entry.Context.AllowsProject(boidReq.ProjectID) {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task identity link is restricted to the current workspace"}
		}
		// task_id が実際にこの workspace に属するかの検証は boid_executor 側で
		// 行う (action_send と同じパターン — broker には TaskStore が
		// 無いため task_id から project を引けない)。
	case BoidOpTaskIdentityUnlink:
		if boidReq.Identity == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task identity unlink requires an identity"}
		}
		if boidReq.ProjectID == "" {
			boidReq.ProjectID = entry.Context.ProjectID
		}
		resolved, err := b.resolveProjectRef(boidReq.ProjectID)
		if err != nil {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task identity unlink: resolve project %q: %s", boidReq.ProjectID, err)}
		}
		boidReq.ProjectID = resolved
		if !entry.Context.AllowsProject(boidReq.ProjectID) {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task identity unlink is restricted to the current workspace"}
		}
	case BoidOpTaskIdentityResolve:
		if boidReq.Identity == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task identity resolve requires an identity"}
		}
		if boidReq.ProjectID == "" {
			boidReq.ProjectID = entry.Context.ProjectID
		}
		resolved, err := b.resolveProjectRef(boidReq.ProjectID)
		if err != nil {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task identity resolve: resolve project %q: %s", boidReq.ProjectID, err)}
		}
		boidReq.ProjectID = resolved
		if !entry.Context.AllowsProject(boidReq.ProjectID) {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task identity resolve is restricted to the current workspace"}
		}
	case BoidOpTaskResolveOrCapture:
		// docs/plans/ingestion-identity.md PR-2 (B-2): scoping is
		// broker-authoritative, matching BoidOpTaskIdentityLink/Resolve
		// exactly (default from ctx, resolve, AllowsProject BEFORE the
		// executor ever sees the request) — see BoidOpTaskResolveOrCapture's
		// own doc comment in protocol.go for why no separate task-ownership
		// check is needed here the way Link needs one for its
		// caller-supplied TaskID.
		if boidReq.Identity == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task resolve-or-capture requires an identity"}
		}
		if boidReq.ProjectID == "" {
			boidReq.ProjectID = entry.Context.ProjectID
		}
		resolved, err := b.resolveProjectRef(boidReq.ProjectID)
		if err != nil {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task resolve-or-capture: resolve project %q: %s", boidReq.ProjectID, err)}
		}
		boidReq.ProjectID = resolved
		if !entry.Context.AllowsProject(boidReq.ProjectID) {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task resolve-or-capture is restricted to the current workspace"}
		}
	case BoidOpActionList:
		// docs/plans/ingestion-identity.md PR-3 (B-3): scoping mirrors
		// BoidOpCardList EXACTLY — same three branches (project_id /
		// workspace_id / neither), same reasons (see BoidOpCardList's
		// case above and BoidOpActionList's own doc comment in protocol.go).
		if boidReq.ProjectID != "" {
			resolved, err := b.resolveProjectRef(boidReq.ProjectID)
			if err != nil {
				return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid action list: resolve project %q: %s", boidReq.ProjectID, err)}
			}
			boidReq.ProjectID = resolved
			if !entry.Context.AllowsProject(boidReq.ProjectID) {
				return &ExecResponse{ExitCode: 1, Stderr: "boid action list: project is outside the current workspace"}
			}
		}
		if boidReq.WorkspaceID != "" && boidReq.WorkspaceID != entry.Context.WorkspaceID {
			return &ExecResponse{ExitCode: 1, Stderr: "boid action list: workspace_id is outside the current workspace"}
		}
		if boidReq.ProjectID == "" && boidReq.WorkspaceID == "" && entry.Context.WorkspaceID != "" {
			boidReq.WorkspaceID = entry.Context.WorkspaceID
		}
	case BoidOpSignalList:
		// docs/plans/signal-ingest-detailed-design.md §3.2 (PR-3): workspace
		// scoping is broker-injected from the job token, never
		// caller-supplied — unlike BoidOpCardList/BoidOpActionList's
		// three-branch project_id/workspace_id/neither shape, there is no
		// flag for this at all (the shim never sets WorkspaceID for this
		// op), so the broker unconditionally overwrites whatever a
		// hand-crafted request set here.
		boidReq.WorkspaceID = entry.Context.WorkspaceID
	case BoidOpSignalAck:
		if len(boidReq.SignalIDs) == 0 {
			return &ExecResponse{ExitCode: 1, Stderr: "boid signal ack requires at least one id"}
		}
		boidReq.WorkspaceID = entry.Context.WorkspaceID
	case BoidOpSignalIngest:
		// Declared for protocol/mirror/escape-manifest completeness (§3.2) —
		// NOT part of the general boidPolicy (policy.go), so the
		// allowsBuiltinOp check above already rejects this for every job in
		// PR-3. This case exists so the scoping shape is ready for PR-5's
		// connector-scoped reduced policy to grant it.
		//
		// [M2, review of PR #1014, 2026-08-26] Service/Connector are
		// overwritten from entry.Context — NEVER trusted from the request
		// as the shim sent it — the SAME "broker-injected, never
		// caller-supplied" pattern WorkspaceID already gets above. Before
		// this fix, a request's Service/Connector were validated for
		// non-emptiness but otherwise passed through verbatim: a
		// well-behaved shim only ever sets them from the
		// BOID_SIGNAL_SERVICE/BOID_SIGNAL_CONNECTOR env, but nothing
		// enforced that server-side, so a hand-crafted ExecRequest
		// bypassing the shim could claim an arbitrary Service/Connector.
		// TokenContext.Service/Connector are empty for every job as of
		// PR-3 (no caller populates them yet — that's PR-5's job when it
		// registers a connector-scoped token), so this op stays rejected
		// in practice regardless; the point of this fix is that the
		// enforcement shape is now actually load-bearing rather than
		// aspirational, so PR-5 doesn't inherit a false sense of security.
		boidReq.Service = entry.Context.Service
		boidReq.Connector = entry.Context.Connector
		if boidReq.Service == "" || boidReq.Connector == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid signal ingest requires a service and connector"}
		}
		if len(boidReq.IngestPayload) > PayloadPatchMaxBytes {
			// Defense in depth: the shim already caps this before ever
			// sending the request (boid_shim.go's parseBoidSignalIngest),
			// but the broker re-checks independently so a shim bypass can't
			// push an oversized payload through to the executor — same
			// two-point pattern as BoidOpTaskUpdatePayloadPatch's
			// PayloadPatch.
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid signal ingest payload exceeds %d bytes", PayloadPatchMaxBytes)}
		}
		boidReq.WorkspaceID = entry.Context.WorkspaceID
	case BoidOpSignalCursorGet:
		// Service/Connector overwrite: same M2 fix as BoidOpSignalIngest
		// above.
		boidReq.Service = entry.Context.Service
		boidReq.Connector = entry.Context.Connector
		if boidReq.Service == "" || boidReq.Connector == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid signal cursor requires a service and connector"}
		}
		boidReq.WorkspaceID = entry.Context.WorkspaceID
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
	// Phase 5b PR1 task-context ops (docs/plans/phase5-shim-and-task-context.md):
	// unlike BoidOpTaskGet (which only defaults an empty TaskID and never
	// rejects a mismatched explicit one), these reject a caller-supplied id
	// that doesn't match the token's own context. The shim never lets an
	// agent target another job's context — id always comes from
	// BOID_TASK_ID/BOID_JOB_ID env, never a CLI flag — so the extra equality
	// check closes an otherwise-pointless cross-task/cross-job read at zero
	// cost to legitimate use.
	//
	// TaskCurrent is TaskID-scoped (it re-derives live from the task row,
	// which carries no job-scoped ambiguity). TaskInstructions, TaskEnv, and
	// TaskPayload are all JobID-scoped: TaskInstructions in particular MUST
	// key off the caller's own job, not its task — two agent-kind hooks for
	// different agents can be dispatched from the same task in one
	// evaluation round (see JobContextSnapshot's doc comment), so a
	// TaskID-only guard would let a claude job read a codex job's
	// instructions (and vice versa) as long as they shared a task. This was
	// caught in codex review on PR #797 before merge — see wiring-seams.md #13.
	case BoidOpTaskCurrent:
		if boidReq.TaskID == "" {
			boidReq.TaskID = entry.Context.TaskID
		}
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task %s requires a task id", taskContextOpVerb(boidReq.Op))}
		}
		if boidReq.TaskID != entry.Context.TaskID {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task %s is restricted to the current task", taskContextOpVerb(boidReq.Op))}
		}
	case BoidOpTaskInstructions, BoidOpTaskEnv, BoidOpTaskPayload:
		if boidReq.JobID == "" {
			boidReq.JobID = entry.Context.JobID
		}
		if boidReq.JobID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task %s requires a job id", taskContextOpVerb(boidReq.Op))}
		}
		if boidReq.JobID != entry.Context.JobID {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task %s is restricted to the current job", taskContextOpVerb(boidReq.Op))}
		}
	// Phase 5b PR2 attachments ops (docs/plans/phase5-shim-and-task-context.md):
	// task-scoped like BoidOpTaskCurrent — attachments belong to the task
	// (not a specific job), so any job dispatched from the task may read
	// them. Same id-equality pattern: default an empty TaskID from the
	// token's own context, then reject a caller-supplied one that mismatches.
	case BoidOpTaskAttachmentsList, BoidOpTaskAttachmentsGet:
		if boidReq.TaskID == "" {
			boidReq.TaskID = entry.Context.TaskID
		}
		if boidReq.TaskID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task attachments %s requires a task id", taskAttachmentsOpVerb(boidReq.Op))}
		}
		if boidReq.TaskID != entry.Context.TaskID {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task attachments %s is restricted to the current task", taskAttachmentsOpVerb(boidReq.Op))}
		}
		if boidReq.Op == BoidOpTaskAttachmentsGet && boidReq.AttachmentName == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task attachments get requires an attachment name"}
		}
	// Phase 5b PR7 (docs/plans/phase5-shim-and-task-context.md): JobID-scoped
	// like BoidOpTaskInstructions/Env/Payload, for the same reason — the
	// merge needs to resolve the calling job's own HandlerID (see
	// api.TaskAppService.UpdateTaskPayloadPatch), which is meaningless
	// without pinning to a specific job.
	case BoidOpTaskUpdatePayloadPatch:
		if boidReq.JobID == "" {
			boidReq.JobID = entry.Context.JobID
		}
		if boidReq.JobID == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task update --payload-patch requires a job id"}
		}
		if boidReq.JobID != entry.Context.JobID {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task update --payload-patch is restricted to the current job"}
		}
		if len(boidReq.PayloadPatch) == 0 {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task update --payload-patch requires a payload patch"}
		}
		// Defense in depth (Phase 5b PR7 codex review Major 3,
		// wiring-seams.md #17): the shim already caps this before ever
		// sending the request (boid_shim.go's readPayloadPatchSource), but
		// the broker re-checks independently so a shim bypass or a future
		// second caller (e.g. a different in-sandbox process crafting the
		// JSON request by hand) can't skip the limit and OOM the daemon.
		if len(boidReq.PayloadPatch) > PayloadPatchMaxBytes {
			return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("boid task update --payload-patch exceeds %d bytes", PayloadPatchMaxBytes)}
		}
	}

	if b.BoidExecutor == nil {
		return &ExecResponse{ExitCode: 1, Stderr: "boid builtin unavailable"}
	}
	return b.BoidExecutor.ExecuteBoidBuiltin(ctx, entry.Context, &boidReq)
}

// taskContextOpVerb renders a Phase 5b PR1 task-context BoidOp as the
// trailing word of its `boid task <verb>` CLI form, for error messages.
func taskContextOpVerb(op BoidOp) string {
	switch op {
	case BoidOpTaskCurrent:
		return "current"
	case BoidOpTaskInstructions:
		return "instructions"
	case BoidOpTaskEnv:
		return "env"
	case BoidOpTaskPayload:
		return "payload"
	default:
		return string(op)
	}
}

// taskAttachmentsOpVerb renders a Phase 5b PR2 attachments BoidOp as the
// trailing word of its `boid task attachments <verb>` CLI form, for error
// messages.
func taskAttachmentsOpVerb(op BoidOp) string {
	switch op {
	case BoidOpTaskAttachmentsList:
		return "list"
	case BoidOpTaskAttachmentsGet:
		return "get"
	default:
		return string(op)
	}
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

// lookupCommand resolves an ExecRequest.Command against a token's registered
// CommandDefs by direct key. As of the 5a-3 cutover
// (docs/plans/phase5-shim-and-task-context.md, "5a: shim 固定ディレクトリ化"
// PR3), every shim's bind-mount basename == its declared short name by
// construction (dispatcher.sandboxShimBinDir + hostCommandSymlinks), and the
// shim always sends that basename as ExecRequest.Command
// (sandbox.CommandFromArgv0). The pre-5a-3 Path-scan fallback that covered
// the absolute-bind-mount-path shape (a rollback safety net for the
// pre-5a-2 shim protocol) is retired here: it was structurally impossible
// to hit once 5a-2 landed and became defense-in-depth against no live
// caller after 5a-3.
func lookupCommand(commands map[string]CommandDef, command string) (CommandDef, bool) {
	def, ok := commands[command]
	return def, ok
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
