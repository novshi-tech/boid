package sandbox_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/mtls"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// TestBrokerTCPListener_MutualTLSHandshake pins docs/plans/phase6-container-backend.md
// §PR4: the broker gains a TCP(mTLS) listener additive to its existing UNIX
// socket, sharing the same handleConn/handle dispatch chain (same
// ExecRequest/ExecResponse protocol, only the transport differs). A client
// presenting a certificate signed by the broker's CA completes the mTLS
// handshake and gets a normal broker response over the TCP transport,
// exactly like the UNIX socket does in TestBroker_ExecCommand.
func TestBrokerTCPListener_MutualTLSHandshake(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate CA: %v", err)
	}
	serverTLSConfig, err := ca.ServerTLSConfig("127.0.0.1")
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	sockPath := filepath.Join(t.TempDir(), "broker.sock")
	broker := &sandbox.Broker{
		SocketPath: sockPath,
		TLSAddr:    "127.0.0.1:0",
		TLSConfig:  serverTLSConfig,
	}

	token := broker.Register(map[string]sandbox.CommandDef{
		"/bin/echo": {
			Name:            "echo",
			Path:            "/bin/echo",
			AllowedPatterns: []string{"*"},
		},
	}, nil, testCtx)
	defer broker.Unregister(token)

	if err := broker.Start(ctx); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	defer broker.Stop()

	tlsAddr := broker.TLSListenAddr()
	if tlsAddr == "" {
		t.Fatal("TLSListenAddr is empty after Start with TLSAddr set")
	}

	// UNIX socket path must still work unchanged (existing socket
	// listener is not removed — §PR4 "既存の UNIX socket listener を削除禁
	// 止").
	unixConnWorks := dialBrokerUnix(t, sockPath, token)
	if !unixConnWorks {
		t.Fatal("UNIX socket path stopped working after adding a TLS listener")
	}

	clientCert, err := ca.IssueClientCert("test-sandbox")
	if err != nil {
		t.Fatalf("IssueClientCert: %v", err)
	}
	clientTLSConfig := ca.ClientTLSConfig("127.0.0.1", clientCert)

	conn, err := tls.Dial("tcp", tlsAddr, clientTLSConfig)
	if err != nil {
		t.Fatalf("tls.Dial: %v", err)
	}
	defer conn.Close()

	req := sandbox.ExecRequest{
		Command: "/bin/echo",
		Args:    []string{"hello", "tls"},
		Token:   token,
	}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	var resp sandbox.ExecResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0; stderr: %s", resp.ExitCode, resp.Stderr)
	}
	if resp.Stdout != "hello tls\n" {
		t.Errorf("stdout = %q, want %q", resp.Stdout, "hello tls\n")
	}
}

// TestBrokerTCPListener_RejectsClientWithoutCertificate pins the "無い接続
// は拒否する" skeleton-mTLS requirement (§PR4): a TLS client that presents
// no certificate at all must not reach the broker's request handling.
func TestBrokerTCPListener_RejectsClientWithoutCertificate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("LoadOrCreate CA: %v", err)
	}
	serverTLSConfig, err := ca.ServerTLSConfig("127.0.0.1")
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}

	sockPath := filepath.Join(t.TempDir(), "broker.sock")
	broker := &sandbox.Broker{
		SocketPath: sockPath,
		TLSAddr:    "127.0.0.1:0",
		TLSConfig:  serverTLSConfig,
	}
	if err := broker.Start(ctx); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	defer broker.Stop()

	// InsecureSkipVerify sidesteps server-cert trust so the failure
	// observed below is specifically the missing client certificate, not
	// an unrelated root-of-trust error.
	clientTLSConfig := &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	conn, dialErr := tls.Dial("tcp", broker.TLSListenAddr(), clientTLSConfig)

	// As in internal/mtls's equivalent test: under TLS 1.3 the client's
	// own Dial can report success even though the server is about to
	// reject the connection (the server validates the client cert
	// requirement only after the client already considers its handshake
	// done). Drive one more round trip to force the rejection to surface.
	var ioErr error
	switch {
	case dialErr != nil:
		ioErr = dialErr
	default:
		defer conn.Close()
		req := sandbox.ExecRequest{Command: "/bin/echo", Token: "whatever"}
		if err := json.NewEncoder(conn).Encode(&req); err != nil {
			ioErr = err
			break
		}
		var resp sandbox.ExecResponse
		ioErr = json.NewDecoder(conn).Decode(&resp)
	}

	if ioErr == nil {
		t.Fatal("broker TLS listener accepted a client with no certificate; want a ClientAuth rejection")
	}
}

// dialBrokerUnix sends one echo request over the broker's UNIX socket and
// reports whether it received a well-formed successful response — used to
// confirm the pre-existing UNIX transport keeps working once a TLS
// listener is layered on top.
func dialBrokerUnix(t *testing.T, sockPath, token string) bool {
	t.Helper()
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial unix broker: %v", err)
	}
	defer conn.Close()

	req := sandbox.ExecRequest{
		Command: "/bin/echo",
		Args:    []string{"still", "unix"},
		Token:   token,
	}
	if err := json.NewEncoder(conn).Encode(&req); err != nil {
		t.Fatalf("encode request: %v", err)
	}
	var resp sandbox.ExecResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp.ExitCode == 0 && resp.Stdout == "still unix\n"
}
