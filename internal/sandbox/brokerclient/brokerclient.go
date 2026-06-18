// Package brokerclient holds the low-level transport for talking to the boid
// broker over its UNIX socket, plus a self-contained JobDone helper.
//
// It is a leaf package: it deliberately does NOT import internal/sandbox so
// that both the sandbox CLI shim (internal/sandbox/boid_shim.go) and the
// go-native sandbox runner (internal/sandbox/runner) can share the transport
// without an import cycle. The wire structs below mirror the JSON shapes the
// broker decodes (internal/sandbox/protocol.go: ExecRequest / ExecResponse /
// BoidRequest) field-for-field; keep the json tags in sync.
package brokerclient

import (
	"encoding/json"
	"fmt"
	"net"
)

// SendJSON dials the broker UNIX socket, JSON-encodes req, and decodes a single
// JSON response into resp. It is the extraction of the former
// sandbox.sendExecRequest transport so the shim and the runner share one
// implementation.
func SendJSON(socket string, req any, resp any) error {
	if socket == "" {
		return fmt.Errorf("broker socket is required")
	}
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return fmt.Errorf("connect to broker: %w", err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	if resp != nil {
		if err := json.NewDecoder(conn).Decode(resp); err != nil {
			return fmt.Errorf("read response: %w", err)
		}
	}
	return nil
}

// execRequest mirrors sandbox.ExecRequest (the subset JobDone needs).
type execRequest struct {
	Command string       `json:"command"`
	Cwd     string       `json:"cwd,omitempty"`
	Token   string       `json:"token"`
	Boid    *boidRequest `json:"boid,omitempty"`
}

// boidRequest mirrors sandbox.BoidRequest (the subset job_done needs).
type boidRequest struct {
	Op       string `json:"op"`
	JobID    string `json:"job_id,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Output   string `json:"output,omitempty"`
}

// execResponse mirrors sandbox.ExecResponse.
type execResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// JobDone posts a `boid job done` builtin request to the broker. It replaces
// the former EXIT-trap `boid job done --exit-code … --output-file …` CLI
// fork-exec: the go runner calls this directly from runner-inner-child.
//
// cwd must be the sandbox working directory (the same cwd the EXIT trap ran in)
// because the broker validates it against the token's project/worktree root
// (validateBoidBuiltinCwd). output carries the agent's payload_patch.json (or
// the stdout-capture fallback); an empty output is valid and matches the bare
// `boid job done --exit-code` form.
func JobDone(socket, token, jobID, cwd string, exitCode int, output []byte) error {
	req := execRequest{
		// Command is unused by the broker for boid builtins (req.Boid != nil
		// short-circuits the dispatch) but mirrors the shim's value for clarity.
		Command: "/usr/local/bin/boid",
		Cwd:     cwd,
		Token:   token,
		Boid: &boidRequest{
			Op:       "job_done",
			JobID:    jobID,
			ExitCode: exitCode,
			Output:   string(output),
		},
	}
	var resp execResponse
	if err := SendJSON(socket, &req, &resp); err != nil {
		return err
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("broker rejected job done: %s", resp.Stderr)
	}
	return nil
}
