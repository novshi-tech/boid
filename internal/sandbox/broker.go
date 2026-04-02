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

// TokenContext carries the job/task/project context associated with a broker token.
type TokenContext struct {
	JobID       string
	TaskID      string
	ProjectID   string
	Role        string
	ProjectDir  string
	WorktreeDir string
}

type tokenEntry struct {
	Context         TokenContext
	Commands        map[string]CommandDef
	BuiltinCommands map[string]struct{}
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

func (b *Broker) Register(commands map[string]CommandDef, builtinCommands []string, ctx TokenContext) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.registry == nil {
		b.registry = make(map[string]*tokenEntry)
	}

	token := generateToken()
	entry := &tokenEntry{
		Context:         ctx,
		Commands:        commands,
		BuiltinCommands: builtinCommandSet(builtinCommands),
	}
	if entry.hasBuiltin("git") {
		var err error
		entry.Git, err = captureGitBinding(ctx.ProjectDir, ctx.WorktreeDir)
		logGitBindingSnapshot(ctx, entry.Git, err)
	}
	b.registry[token] = entry
	return token
}

type SecretResolver func(key string) (string, error)

func (b *Broker) RegisterWithSecrets(commands map[string]CommandDef, builtinCommands []string, ctx TokenContext, resolver SecretResolver) string {
	resolved := make(map[string]CommandDef, len(commands))
	for name, def := range commands {
		if len(def.Env) > 0 {
			newEnv := make(map[string]string, len(def.Env))
			for k, v := range def.Env {
				if strings.HasPrefix(v, "secret:") {
					secretKey := v[len("secret:"):]
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
	return b.Register(resolved, builtinCommands, ctx)
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
		return handleGitBuiltinRequest(req, entry)
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
	if !entry.hasBuiltin("boid") {
		return &ExecResponse{ExitCode: 1, Stderr: "command not allowed: boid"}
	}

	if err := validateBoidBuiltinCwd(req.Cwd, entry); err != nil {
		return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
	}

	boidReq := *req.Boid
	switch entry.Context.Role {
	case "hook":
		if boidReq.Op != BoidOpJobDone {
			return &ExecResponse{
				ExitCode: 1,
				Stderr:   fmt.Sprintf("boid op %q not allowed for role %s", boidReq.Op, entry.Context.Role),
			}
		}
	case "gate":
		if boidReq.Op != BoidOpJobDone && boidReq.Op != BoidOpTaskCreate {
			return &ExecResponse{
				ExitCode: 1,
				Stderr:   fmt.Sprintf("boid op %q not allowed for role %s", boidReq.Op, entry.Context.Role),
			}
		}
	default:
		if boidReq.Op != BoidOpJobDone && boidReq.Op != BoidOpTaskCreate {
			return &ExecResponse{
				ExitCode: 1,
				Stderr:   fmt.Sprintf("boid op %q not allowed for role %s", boidReq.Op, entry.Context.Role),
			}
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

	if entry != nil && entry.Context.Role == "gate" && cwd == "/tmp" {
		return nil
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

func builtinCommandSet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		out[name] = struct{}{}
	}
	return out
}

func (e *tokenEntry) hasBuiltin(name string) bool {
	if e == nil || len(e.BuiltinCommands) == 0 {
		return false
	}
	_, ok := e.BuiltinCommands[name]
	return ok
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
