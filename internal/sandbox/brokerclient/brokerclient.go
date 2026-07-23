// Package brokerclient holds the low-level transport for talking to the boid
// broker over its UNIX socket or its TCP(mTLS) listener, plus a
// self-contained JobDone helper.
//
// It is a leaf package: it deliberately does NOT import internal/sandbox so
// that both the sandbox CLI shim (internal/sandbox/boid_shim.go) and the
// go-native sandbox runner (internal/sandbox/runner) can share the transport
// without an import cycle. The wire structs below mirror the JSON shapes the
// broker decodes (internal/sandbox/protocol.go: ExecRequest / ExecResponse /
// BoidRequest) field-for-field; keep the json tags in sync.
//
// Two transports (docs/plans/phase6-cutover-followups.md §⓪ "broker TCP
// wire completion"): SendJSON dials the broker's UNIX socket — the original,
// still-current mechanism for the userns backend, where the sandbox and the
// daemon share a mount namespace so a bind-mounted socket file is always
// reachable. SendJSONTLS dials the broker's TCP(mTLS) listener instead — the
// only mechanism a container-backend job's SIBLING container can use, since
// it has no access to the daemon container's own filesystem at all. Every
// real caller in this repo picks between the two via SendJSONFromEnv (or,
// for JobDone, the equivalent map-driven decision) rather than choosing a
// transport itself — see its own doc comment for the exact selection rule.
package brokerclient

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"os"
)

// Env var names shared between the container backend's delivery side
// (internal/dispatcher/container_backend.go's withBrokerTLSEnv, which sets
// these five plus BOID_BROKER_SOCKET's own separate, backend-agnostic
// mount) and this package's own transport-selection logic
// (SendJSONFromEnv). Exported so a caller that already has these values in
// hand some other way (e.g. a test) can reference the same literal keys
// instead of hand-typing them a second time.
const (
	EnvBrokerSocket        = "BOID_BROKER_SOCKET"
	EnvBrokerTLSAddr       = "BOID_BROKER_TLS_ADDR"
	EnvBrokerTLSCertPath   = "BOID_BROKER_TLS_CERT_PATH"
	EnvBrokerTLSKeyPath    = "BOID_BROKER_TLS_KEY_PATH"
	EnvBrokerTLSCAPath     = "BOID_BROKER_TLS_CA_PATH"
	EnvBrokerTLSServerName = "BOID_BROKER_TLS_SERVER_NAME"
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

// SendJSONTLS dials the broker's TCP(mTLS) listener at addr, presenting the
// client certificate/key at certPath/keyPath, trusting the CA certificate at
// caPath, and verifying the server's own certificate against serverName —
// the TCP+mTLS counterpart to SendJSON's plain UNIX dial. This is the
// transport a container-backend job's sibling container uses (docs/plans/
// phase6-cutover-followups.md §⓪): the per-job client cert/key/ca trio
// certPath/keyPath/caPath name is bind-mounted read-only by
// internal/dispatcher/container_backend.go's Launch at a fixed
// container-internal path (containerBrokerTLSDir), and addr/serverName are
// the compose service DNS name (+ port) and SAN the broker's own
// mtls.CA.ServerTLSConfig("127.0.0.1", "localhost",
// composeBrokerServiceName) call issued its listener cert for — see
// internal/server/server.go's Start.
//
// Real callers should not build this argument list by hand — SendJSONFromEnv
// (or, for JobDone, its own equivalent) reads the exact same five values out
// of the caller's environment and is the actual production entry point.
// This function is exported mainly so it is independently unit-testable
// against a real tls.Listen-based fake server (see brokerclient_test.go).
func SendJSONTLS(addr, serverName, certPath, keyPath, caPath string, req any, resp any) error {
	conn, err := dialTLS(addr, serverName, certPath, keyPath, caPath)
	if err != nil {
		return fmt.Errorf("connect to broker (tls): %w", err)
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

// dialTLS is SendJSONTLS's own connection-establishment step, factored out
// so DialFromEnv (a caller that needs the raw net.Conn for a streaming
// protocol, not one JSON request/response round trip — see its own doc
// comment) shares the exact same cert/key/CA loading and tls.Config
// construction instead of a second hand-maintained copy.
func dialTLS(addr, serverName, certPath, keyPath, caPath string) (net.Conn, error) {
	if addr == "" {
		return nil, fmt.Errorf("broker tls address is required")
	}
	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, fmt.Errorf("load broker client cert: %w", err)
	}
	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("read broker ca cert: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse broker ca cert: no valid PEM certificate found in %s", caPath)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   tls.VersionTLS12,
	}
	return tls.Dial("tcp", addr, tlsCfg)
}

// DialFromEnv dials the broker using the exact same transport-selection
// rule as SendJSONFromEnv (TLS when BOID_BROKER_TLS_ADDR is set, else UNIX
// when BOID_BROKER_SOCKET is set), but returns the raw net.Conn instead of
// performing one JSON request/response round trip itself. This is for a
// caller that needs a long-lived connection for its own streaming protocol
// — internal/sandbox/shim.go's sendStreamingExecRequest (host-command exec
// with real-time stdout/stderr forwarding and signal propagation, which
// predates and is not JSON-request/response-shaped the way SendJSON's
// protocol is) is the one caller in this repo.
func DialFromEnv() (net.Conn, error) {
	if addr := os.Getenv(EnvBrokerTLSAddr); addr != "" {
		conn, err := dialTLS(addr,
			os.Getenv(EnvBrokerTLSServerName),
			os.Getenv(EnvBrokerTLSCertPath),
			os.Getenv(EnvBrokerTLSKeyPath),
			os.Getenv(EnvBrokerTLSCAPath))
		if err != nil {
			return nil, fmt.Errorf("connect to broker (tls): %w", err)
		}
		return conn, nil
	}
	if socket := os.Getenv(EnvBrokerSocket); socket != "" {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			return nil, fmt.Errorf("connect to broker: %w", err)
		}
		return conn, nil
	}
	return nil, fmt.Errorf("brokerclient: neither %s nor %s is set", EnvBrokerTLSAddr, EnvBrokerSocket)
}

// sendJSONWithLookup is the transport-selection decision point
// SendJSONFromEnv and JobDone both delegate to, parameterized over how a
// caller looks up a named var: SendJSONFromEnv passes os.Getenv (the
// shim — a real subprocess whose OS environment carries these vars, set by
// docker create's Config.Env or spec.Env's direct-exec overlay — see
// adapters/claude/run.go's own doc comment on that overlay); JobDone passes
// a lookup closed over a sandbox.Spec.Env map instead (the go-native
// runner never re-execs through a subprocess for this decision — spec.Env
// is read directly out of the JSON spec this process was launched with, so
// os.Getenv would not necessarily reflect it).
//
// Selection rule: TLS (SendJSONTLS) when BOID_BROKER_TLS_ADDR is non-empty
// — the container backend's own delivery mechanism — otherwise UNIX
// (SendJSON) when BOID_BROKER_SOCKET is non-empty — the userns backend's
// unchanged mechanism, and every other pre-this-feature caller/test. TLS
// wins when both are set (see EnvBrokerTLSAddr's own const-block doc
// comment for why a container-backend job coincidentally also carrying a
// stale/unreachable BOID_BROKER_SOCKET value is not a conflict this
// function needs to resolve any other way). Neither set is a genuine error
// case, not a silent no-op: every non-ProfileInit, non-foreground job has
// SOME broker transport (internal/dispatcher/runner.go's own broker
// registration gate) by the time either JobDone or the shim's entry point
// runs, so an empty lookup here means broker wiring is broken, and the
// caller needs to know.
func sendJSONWithLookup(lookup func(string) string, req, resp any) error {
	if addr := lookup(EnvBrokerTLSAddr); addr != "" {
		return SendJSONTLS(addr,
			lookup(EnvBrokerTLSServerName),
			lookup(EnvBrokerTLSCertPath),
			lookup(EnvBrokerTLSKeyPath),
			lookup(EnvBrokerTLSCAPath),
			req, resp)
	}
	if socket := lookup(EnvBrokerSocket); socket != "" {
		return SendJSON(socket, req, resp)
	}
	return fmt.Errorf("brokerclient: neither %s nor %s is set", EnvBrokerTLSAddr, EnvBrokerSocket)
}

// SendJSONFromEnv is the single decision point every real shim caller in
// this repo shares for picking a broker transport based on the CURRENT
// PROCESS's own environment — see sendJSONWithLookup's own doc comment for
// the exact selection rule. This is what lets internal/sandbox/shim.go's
// sendExecRequest (and everything that calls it: RunBoidShim, `boid fetch`,
// the task-context/attachments subcommands) stay backend-agnostic without
// each duplicating the TLS-vs-UNIX decision itself.
func SendJSONFromEnv(req any, resp any) error {
	return sendJSONWithLookup(os.Getenv, req, resp)
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
// fork-exec: the go runner calls this directly from runner-inner-child /
// RunContainer.
//
// env is the job's own env view (internal/sandbox/runner.postJobDone passes
// spec.Env directly, not os.Environ() — see sendJSONWithLookup's own doc
// comment for why) — JobDone picks TLS vs UNIX from it via the exact same
// rule SendJSONFromEnv applies to the current process's real environment.
// Before docs/plans/phase6-cutover-followups.md §⓪ this parameter was a
// bare `socket string`; every caller (there is exactly one,
// internal/sandbox/runner/runner.go's postJobDone) now passes spec.Env
// wholesale instead of extracting BOID_BROKER_SOCKET itself, so a
// container-backend job's BOID_BROKER_TLS_* keys are visible to this
// function too.
//
// cwd must be the sandbox working directory (the same cwd the EXIT trap ran in)
// because the broker validates it against the token's project/worktree root
// (validateBoidBuiltinCwd). output carries the agent's payload_patch.json (or
// the stdout-capture fallback); an empty output is valid and matches the bare
// `boid job done --exit-code` form.
func JobDone(env map[string]string, token, jobID, cwd string, exitCode int, output []byte) error {
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
	lookup := func(key string) string { return env[key] }
	if err := sendJSONWithLookup(lookup, &req, &resp); err != nil {
		return err
	}
	if resp.ExitCode != 0 {
		return fmt.Errorf("broker rejected job done: %s", resp.Stderr)
	}
	return nil
}
