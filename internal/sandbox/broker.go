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
	JobID     string
	TaskID    string
	ProjectID string
	Role      string
}

type tokenEntry struct {
	Context  TokenContext
	Commands map[string]CommandDef
}

type Broker struct {
	SocketPath string
	BoidBinary string
	listener   net.Listener
	mu         sync.RWMutex
	registry   map[string]*tokenEntry
}

func (b *Broker) Register(commands map[string]CommandDef, ctx TokenContext) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.registry == nil {
		b.registry = make(map[string]*tokenEntry)
	}

	token := generateToken()
	b.registry[token] = &tokenEntry{
		Context:  ctx,
		Commands: commands,
	}
	return token
}

type SecretResolver func(key string) (string, error)

func (b *Broker) RegisterWithSecrets(commands map[string]CommandDef, ctx TokenContext, resolver SecretResolver) string {
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
	return b.Register(resolved, ctx)
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

	def, ok := entry.Commands[req.Command]
	if !ok {
		return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("command not allowed: %s", req.Command)}
	}

	return b.execCommand(req, def)
}

func (b *Broker) handleBoidBuiltin(req *ExecRequest, entry *tokenEntry) *ExecResponse {
	subcmd := extractSimpleSubcommand(req.Args)

	allowed := false
	switch entry.Context.Role {
	case "hook":
		allowed = subcmd == "job"
	case "gate":
		allowed = subcmd == "job" || subcmd == "task"
	}

	if !allowed {
		return &ExecResponse{
			ExitCode: 1,
			Stderr:   fmt.Sprintf("boid subcommand %q not allowed for role %s", subcmd, entry.Context.Role),
		}
	}

	if subcmd == "task" && len(req.Args) > 1 && req.Args[1] == "create" && entry.Context.ProjectID != "" {
		hasProject := false
		for _, a := range req.Args {
			if a == "--project" {
				hasProject = true
				break
			}
		}
		if !hasProject {
			req.Args = append(req.Args, "--project", entry.Context.ProjectID)
		}
	}

	boidPath := b.BoidBinary
	if boidPath == "" {
		var err error
		boidPath, err = exec.LookPath("boid")
		if err != nil {
			return &ExecResponse{ExitCode: 1, Stderr: "boid binary not found"}
		}
	}

	def := CommandDef{
		Name:            "boid",
		Path:            boidPath,
		AllowedPatterns: []string{"*"},
	}

	return b.execCommand(req, def)
}

func (b *Broker) execCommand(req *ExecRequest, def CommandDef) *ExecResponse {
	if def.RequireCwd {
		if req.Cwd == "" {
			return &ExecResponse{ExitCode: 1, Stderr: "cwd required"}
		}
		if !filepath.IsAbs(req.Cwd) {
			return &ExecResponse{ExitCode: 1, Stderr: "cwd must be absolute"}
		}
		if len(def.AllowedCwdPrefixes) > 0 {
			allowed := false
			for _, prefix := range def.AllowedCwdPrefixes {
				if req.Cwd == prefix || strings.HasPrefix(req.Cwd, prefix+"/") {
					allowed = true
					break
				}
			}
			if !allowed {
				return &ExecResponse{ExitCode: 1, Stderr: "cwd not in allowed prefixes"}
			}
		}
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

func generateToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
