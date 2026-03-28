package hostcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
)

type Broker struct {
	SocketPath string
	Commands   map[string]CommandDef
	listener   net.Listener
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
	def, ok := b.Commands[req.Command]
	if !ok {
		return &ExecResponse{ExitCode: 1, Stderr: fmt.Sprintf("command not allowed: %s", req.Command)}
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

	out, err := cmd.CombinedOutput()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			return &ExecResponse{ExitCode: 1, Stderr: err.Error()}
		}
	}

	return &ExecResponse{ExitCode: exitCode, Stdout: string(out)}
}
