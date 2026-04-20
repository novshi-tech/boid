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
	Git             *GitBinding
}

type Broker struct {
	SocketPath      string
	BoidBinary      string
	BoidExecutor    BoidExecutor
	ProjectResolver ProjectResolver
	listener        net.Listener
	mu              sync.RWMutex
	registry        map[string]*tokenEntry
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
	if entry.hasBuiltinPolicy("git") {
		var err error
		entry.Git, err = captureGitBinding(ctx.ProjectDir, ctx.WorktreeDir)
		logGitBindingSnapshot(ctx, entry.Git, err)
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
			for k, v := range def.Env {
				if strings.HasPrefix(v, "secret:") {
					secretKey := v[len("secret:"):]
					if secretKey == "" {
						secretKey = k // env var name as secret key
					}
					val, err := resolver(secretKey)
					if err != nil {
						slog.Warn("failed to resolve secret", "key", secretKey, "error", err)
						continue
					}
					newEnv[k] = val
				} else {
					newEnv[k] = v
				}
			}
			def.Env = newEnv
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

	resp := b.Handle(&req)
	json.NewEncoder(conn).Encode(resp)
}

func (b *Broker) Handle(req *ExecRequest) *ExecResponse {
	b.mu.RLock()
	entry, ok := b.registry[req.Token]
	b.mu.RUnlock()

	if !ok {
		return &ExecResponse{ExitCode: 1, Stderr: "invalid token"}
	}

	if req.Command == "boid" {
		return b.handleBoidBuiltin(req, entry)
	}
	if req.Command == "git" {
		if entry.hasBuiltinPolicy("git") {
			return handleGitBuiltinRequest(req, entry)
		}
		if def, ok := entry.Commands["git"]; ok {
			return b.execCommand(req, def, entry)
		}
		return &ExecResponse{ExitCode: 1, Stderr: "command not allowed: git"}
	}

	def, ok := entry.Commands[req.Command]
	if !ok {
		return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("command not allowed: %s", req.Command)}
	}

	return b.execCommand(req, def, entry)
}

func (b *Broker) handleBoidBuiltin(req *ExecRequest, entry *tokenEntry) *ExecResponse {
	if req.Boid == nil {
		return &ExecResponse{ExitCode: 1, Stderr: "typed boid request required"}
	}
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
	return b.BoidExecutor.ExecuteBoidBuiltin(entry.Context, &boidReq)
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

func entryRoot(entry *tokenEntry) string {
	if entry == nil {
		return ""
	}
	if entry.Context.WorktreeDir != "" {
		return entry.Context.WorktreeDir
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
	if err := validateStdin(def, req.Stdin); err != nil {
		return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
	}

	if !CheckPolicy(def, req.Args) {
		return &ExecResponse{ExitCode: 1, Stderr: "arguments not allowed"}
	}

	cmd := exec.Command(def.Path, req.Args...)
	cwd := resolveHostCommandCwd(req.Cwd, entry)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}

	if len(def.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range def.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}

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

// resolveHostCommandCwd decides the working directory for a host command.
// Host commands run on the host, not inside the sandbox. The sandbox-side cwd
// (req.Cwd) aligns with host-side paths for hook jobs (worktree is mounted at
// the same path inside and outside the sandbox) but not for gate jobs, where
// the sandbox cwd falls through to a tmpfs HOME — on the host that path is
// the user's real HOME and carries no repo metadata.
//
// The token's host-side context (WorktreeDir / ProjectDir) is always
// independent of sandbox visibility (Visibility.ProjectDir), so we can lean
// on it directly: prefer the task worktree, then the project work dir, then
// fall back to what the sandbox reported.
func resolveHostCommandCwd(requestedCwd string, entry *tokenEntry) string {
	if entry != nil {
		if entry.Context.WorktreeDir != "" {
			return entry.Context.WorktreeDir
		}
		if entry.Context.ProjectDir != "" {
			return entry.Context.ProjectDir
		}
	}
	return requestedCwd
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

func validateStdin(def CommandDef, stdin []byte) error {
	if len(stdin) > 0 && !def.AllowStdin {
		return fmt.Errorf("stdin not allowed")
	}
	return nil
}

func generateToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
