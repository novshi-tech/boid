package hostcmd

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

type Broker struct {
	SocketPath string
	listener   net.Listener
	mu         sync.RWMutex
	registry   map[string]map[string]CommandDef // token -> command name -> def
}

// Register registers a set of commands for a new token and returns the token.
func (b *Broker) Register(commands map[string]CommandDef) string {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.registry == nil {
		b.registry = make(map[string]map[string]CommandDef)
	}

	token := generateToken()
	b.registry[token] = commands
	return token
}

// SecretResolver resolves a secret key to its value.
type SecretResolver func(key string) (string, error)

// RegisterWithSecrets registers commands and resolves secret: prefixed env values.
func (b *Broker) RegisterWithSecrets(commands map[string]CommandDef, resolver SecretResolver) string {
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
	return b.Register(resolved)
}

// Unregister removes the command set associated with the given token.
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
	commands, ok := b.registry[req.Token]
	b.mu.RUnlock()

	if !ok {
		return &ExecResponse{ExitCode: 1, Stderr: "invalid token"}
	}

	def, ok := commands[req.Command]
	if !ok {
		return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("command not allowed: %s", req.Command)}
	}

	// cwd validation
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

	// Inherit host env and overlay command-specific env
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
