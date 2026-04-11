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
	SocketPath   string
	BoidBinary   string
	BoidExecutor BoidExecutor
	listener     net.Listener
	mu           sync.RWMutex
	registry     map[string]*tokenEntry
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
			return b.execCommand(req, def)
		}
		return &ExecResponse{ExitCode: 1, Stderr: "command not allowed: git"}
	}

	def, ok := entry.Commands[req.Command]
	if !ok {
		return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("command not allowed: %s", req.Command)}
	}

	return b.execCommand(req, def)
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
			Stderr:   fmt.Sprintf("boid op %q not allowed for role %s", boidReq.Op, entry.Context.Role),
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
	case BoidOpTaskImport:
		if len(boidReq.ImportTasks) == 0 {
			return &ExecResponse{ExitCode: 1, Stderr: "boid task import requires at least one task"}
		}
		// バッチ全体の project_id 事前検証
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

	if entry != nil && entry.Context.Role == "gate" {
		if cwd == "/tmp" {
			return nil
		}
		if entry.Context.ProjectDir != "" && isWithinRoot(cwd, entry.Context.ProjectDir) {
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

func (b *Broker) execCommand(req *ExecRequest, def CommandDef) *ExecResponse {
	if err := validateStdin(def, req.Stdin); err != nil {
		return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
	}

	if !CheckPolicy(def, req.Args) {
		return &ExecResponse{ExitCode: 1, Stderr: "arguments not allowed"}
	}

	cmd := exec.Command(def.Path, req.Args...)
	if req.Cwd != "" {
		cmd.Dir = req.Cwd
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
