package brokerclient

import (
	"crypto/tls"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/mtls"
)

// fakeBroker accepts one connection, decodes the request into a generic map,
// records it, and replies with the given exit code / stderr.
type fakeBroker struct {
	socket   string
	ln       net.Listener
	requests chan map[string]any
	respCode int
	respErr  string
}

func startFakeBroker(t *testing.T, respCode int, respErr string) *fakeBroker {
	t.Helper()
	socket := filepath.Join(t.TempDir(), "broker.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &fakeBroker{socket: socket, ln: ln, requests: make(chan map[string]any, 1), respCode: respCode, respErr: respErr}
	go b.serve()
	t.Cleanup(func() { ln.Close() })
	return b
}

func (b *fakeBroker) serve() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		var req map[string]any
		if err := json.NewDecoder(conn).Decode(&req); err == nil {
			select {
			case b.requests <- req:
			default:
			}
		}
		_ = json.NewEncoder(conn).Encode(map[string]any{
			"exit_code": b.respCode,
			"stderr":    b.respErr,
		})
		conn.Close()
	}
}

// fakeTLSBroker is fakeBroker's TCP+mTLS counterpart, backed by a real
// mtls.CA-issued server certificate (mirrors the mTLS test harness pattern
// internal/sandbox/broker_tls_test.go already establishes for the broker's
// own package — this package cannot import internal/sandbox at all, being
// a leaf dependency of it, so the harness is reproduced here rather than
// shared).
type fakeTLSBroker struct {
	addr     string
	ln       net.Listener
	requests chan map[string]any
	respCode int
	respErr  string
}

func startFakeTLSBroker(t *testing.T, ca *mtls.CA, respCode int, respErr string) *fakeTLSBroker {
	t.Helper()
	serverTLSCfg, err := ca.ServerTLSConfig("127.0.0.1")
	if err != nil {
		t.Fatalf("ServerTLSConfig: %v", err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", serverTLSCfg)
	if err != nil {
		t.Fatalf("tls.Listen: %v", err)
	}
	b := &fakeTLSBroker{addr: ln.Addr().String(), ln: ln, requests: make(chan map[string]any, 1), respCode: respCode, respErr: respErr}
	go b.serve()
	t.Cleanup(func() { ln.Close() })
	return b
}

func (b *fakeTLSBroker) serve() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			var req map[string]any
			if err := json.NewDecoder(conn).Decode(&req); err == nil {
				select {
				case b.requests <- req:
				default:
				}
			}
			_ = json.NewEncoder(conn).Encode(map[string]any{
				"exit_code": b.respCode,
				"stderr":    b.respErr,
			})
		}()
	}
}

// writeClientCertFiles issues a client cert from ca and writes the
// cert.pem/key.pem/ca.pem trio to a fresh temp dir, in the exact layout
// SendJSONTLS/DialFromEnv (and, in production, containerBackend.
// materializeBrokerClientCert) expect — returns the three file paths.
func writeClientCertFiles(t *testing.T, ca *mtls.CA) (certPath, keyPath, caPath string) {
	t.Helper()
	leaf, err := ca.IssueClientCert("test-client")
	if err != nil {
		t.Fatalf("IssueClientCert: %v", err)
	}
	certPEM, keyPEM, err := mtls.EncodeCertPEM(leaf)
	if err != nil {
		t.Fatalf("EncodeCertPEM: %v", err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	caPath = filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatalf("write cert.pem: %v", err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatalf("write key.pem: %v", err)
	}
	if err := os.WriteFile(caPath, ca.CertPEM(), 0o600); err != nil {
		t.Fatalf("write ca.pem: %v", err)
	}
	return certPath, keyPath, caPath
}

func TestJobDone_WireFormat(t *testing.T) {
	b := startFakeBroker(t, 0, "")

	err := JobDone(map[string]string{EnvBrokerSocket: b.socket}, "tok-123", "job-7", "/work/dir", 0, []byte(`{"artifact":{}}`))
	if err != nil {
		t.Fatalf("JobDone: %v", err)
	}

	req := <-b.requests
	if req["token"] != "tok-123" {
		t.Errorf("token = %v, want tok-123", req["token"])
	}
	if req["cwd"] != "/work/dir" {
		t.Errorf("cwd = %v, want /work/dir", req["cwd"])
	}
	boid, ok := req["boid"].(map[string]any)
	if !ok {
		t.Fatalf("boid payload missing or wrong type: %v", req["boid"])
	}
	if boid["op"] != "job_done" {
		t.Errorf("op = %v, want job_done", boid["op"])
	}
	if boid["job_id"] != "job-7" {
		t.Errorf("job_id = %v, want job-7", boid["job_id"])
	}
	if boid["output"] != `{"artifact":{}}` {
		t.Errorf("output = %v, want the payload patch", boid["output"])
	}
}

func TestJobDone_NonZeroExitCodePreserved(t *testing.T) {
	b := startFakeBroker(t, 0, "")
	if err := JobDone(map[string]string{EnvBrokerSocket: b.socket}, "t", "j", "/w", 42, nil); err != nil {
		t.Fatalf("JobDone: %v", err)
	}
	req := <-b.requests
	boid := req["boid"].(map[string]any)
	// JSON numbers decode as float64.
	if boid["exit_code"].(float64) != 42 {
		t.Errorf("exit_code = %v, want 42", boid["exit_code"])
	}
}

func TestJobDone_BrokerRejection(t *testing.T) {
	b := startFakeBroker(t, 1, "boid op \"job_done\" not allowed by policy")
	err := JobDone(map[string]string{EnvBrokerSocket: b.socket}, "t", "j", "/w", 0, nil)
	if err == nil {
		t.Fatal("expected error when broker rejects job done")
	}
}

// TestJobDone_NeitherSocketNorTLSAddr_Errors pins JobDone's own use of the
// shared sendJSONWithLookup decision point (docs/plans/
// phase6-cutover-followups.md §⓪): an env map with neither key set is a
// real error, not a silent no-op — internal/sandbox/runner.postJobDone's
// own call site checks for this case itself before ever calling JobDone
// (see its own doc comment), but JobDone must not paper over it either if
// some other caller skips that pre-check.
func TestJobDone_NeitherSocketNorTLSAddr_Errors(t *testing.T) {
	err := JobDone(map[string]string{}, "t", "j", "/w", 0, nil)
	if err == nil {
		t.Fatal("expected an error when the env map carries neither BOID_BROKER_SOCKET nor BOID_BROKER_TLS_ADDR")
	}
}

// TestJobDone_ChoosesTLSFromEnvMap pins the map-driven (not os.Getenv-driven)
// half of JobDone's transport selection: a container-backend job's spec.Env
// carrying BOID_BROKER_TLS_* keys (and no BOID_BROKER_SOCKET at all) must
// route through SendJSONTLS, exactly as the shim's SendJSONFromEnv does for
// the equivalent case in the current process's real environment.
func TestJobDone_ChoosesTLSFromEnvMap(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	tlsBroker := startFakeTLSBroker(t, ca, 0, "")
	certPath, keyPath, caPath := writeClientCertFiles(t, ca)

	env := map[string]string{
		EnvBrokerTLSAddr:       tlsBroker.addr,
		EnvBrokerTLSServerName: "127.0.0.1",
		EnvBrokerTLSCertPath:   certPath,
		EnvBrokerTLSKeyPath:    keyPath,
		EnvBrokerTLSCAPath:     caPath,
	}
	if err := JobDone(env, "tok-tls", "job-tls", "/work/dir", 0, nil); err != nil {
		t.Fatalf("JobDone: %v", err)
	}

	req := <-tlsBroker.requests
	if req["token"] != "tok-tls" {
		t.Errorf("token = %v, want tok-tls", req["token"])
	}
}

// TestSendJSONTLS_WireFormat pins the round trip itself: a real TLS dial
// against a tls.Listen-based fake server backed by mtls.CA.ServerTLSConfig,
// presenting a cert this same CA issued, completes the handshake and
// exchanges the exact same JSON wire shape SendJSON's UNIX transport does.
func TestSendJSONTLS_WireFormat(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	b := startFakeTLSBroker(t, ca, 0, "")
	certPath, keyPath, caPath := writeClientCertFiles(t, ca)

	type req struct {
		Command string `json:"command"`
	}
	type resp struct {
		ExitCode int `json:"exit_code"`
	}
	var out resp
	if err := SendJSONTLS(b.addr, "127.0.0.1", certPath, keyPath, caPath, &req{Command: "echo"}, &out); err != nil {
		t.Fatalf("SendJSONTLS: %v", err)
	}
	if out.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", out.ExitCode)
	}

	got := <-b.requests
	if got["command"] != "echo" {
		t.Errorf("command = %v, want echo", got["command"])
	}
}

func TestSendJSONTLS_EmptyAddr_Errors(t *testing.T) {
	err := SendJSONTLS("", "127.0.0.1", "/nonexistent/cert.pem", "/nonexistent/key.pem", "/nonexistent/ca.pem", map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected an error for an empty address")
	}
}

func TestSendJSONTLS_BadCertPath_Errors(t *testing.T) {
	err := SendJSONTLS("127.0.0.1:1", "127.0.0.1", "/nonexistent/cert.pem", "/nonexistent/key.pem", "/nonexistent/ca.pem", map[string]any{}, nil)
	if err == nil {
		t.Fatal("expected an error for a missing client cert file")
	}
}

// TestSendJSONFromEnv_TLSBranch pins SendJSONFromEnv's TLS selection: with
// BOID_BROKER_TLS_ADDR set (and no BOID_BROKER_SOCKET at all), it must dial
// the TLS listener, not attempt a UNIX dial.
func TestSendJSONFromEnv_TLSBranch(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	tlsBroker := startFakeTLSBroker(t, ca, 0, "")
	certPath, keyPath, caPath := writeClientCertFiles(t, ca)

	t.Setenv(EnvBrokerTLSAddr, tlsBroker.addr)
	t.Setenv(EnvBrokerTLSServerName, "127.0.0.1")
	t.Setenv(EnvBrokerTLSCertPath, certPath)
	t.Setenv(EnvBrokerTLSKeyPath, keyPath)
	t.Setenv(EnvBrokerTLSCAPath, caPath)
	t.Setenv(EnvBrokerSocket, "") // explicitly unset

	req := map[string]any{"command": "from-env-tls"}
	var resp map[string]any
	if err := SendJSONFromEnv(&req, &resp); err != nil {
		t.Fatalf("SendJSONFromEnv: %v", err)
	}

	got := <-tlsBroker.requests
	if got["command"] != "from-env-tls" {
		t.Errorf("command = %v, want from-env-tls", got["command"])
	}
}

// TestSendJSONFromEnv_UnixBranch pins the unchanged userns-backend path:
// with only BOID_BROKER_SOCKET set, SendJSONFromEnv must dial the UNIX
// socket exactly as SendJSON always has.
func TestSendJSONFromEnv_UnixBranch(t *testing.T) {
	b := startFakeBroker(t, 0, "")
	t.Setenv(EnvBrokerTLSAddr, "")
	t.Setenv(EnvBrokerSocket, b.socket)

	req := map[string]any{"command": "from-env-unix"}
	var resp map[string]any
	if err := SendJSONFromEnv(&req, &resp); err != nil {
		t.Fatalf("SendJSONFromEnv: %v", err)
	}

	got := <-b.requests
	if got["command"] != "from-env-unix" {
		t.Errorf("command = %v, want from-env-unix", got["command"])
	}
}

// TestSendJSONFromEnv_TLSPreferredOverUnix pins the priority rule when
// BOTH env vars happen to be set: TLS wins, exercised against two DISTINCT
// fake servers so "which one actually received the request" is
// unambiguous (not just "which one didn't error").
func TestSendJSONFromEnv_TLSPreferredOverUnix(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	tlsBroker := startFakeTLSBroker(t, ca, 0, "")
	certPath, keyPath, caPath := writeClientCertFiles(t, ca)
	unixBroker := startFakeBroker(t, 0, "")

	t.Setenv(EnvBrokerTLSAddr, tlsBroker.addr)
	t.Setenv(EnvBrokerTLSServerName, "127.0.0.1")
	t.Setenv(EnvBrokerTLSCertPath, certPath)
	t.Setenv(EnvBrokerTLSKeyPath, keyPath)
	t.Setenv(EnvBrokerTLSCAPath, caPath)
	t.Setenv(EnvBrokerSocket, unixBroker.socket)

	req := map[string]any{"command": "priority-check"}
	var resp map[string]any
	if err := SendJSONFromEnv(&req, &resp); err != nil {
		t.Fatalf("SendJSONFromEnv: %v", err)
	}

	select {
	case got := <-tlsBroker.requests:
		if got["command"] != "priority-check" {
			t.Errorf("command = %v, want priority-check", got["command"])
		}
	default:
		t.Fatal("TLS broker did not receive the request; want TLS to win when both BOID_BROKER_TLS_ADDR and BOID_BROKER_SOCKET are set")
	}
	select {
	case got := <-unixBroker.requests:
		t.Errorf("UNIX broker unexpectedly received a request too: %v", got)
	default:
	}
}

func TestSendJSONFromEnv_NeitherSet_Errors(t *testing.T) {
	t.Setenv(EnvBrokerTLSAddr, "")
	t.Setenv(EnvBrokerSocket, "")

	req := map[string]any{}
	err := SendJSONFromEnv(&req, nil)
	if err == nil {
		t.Fatal("expected an error when neither BOID_BROKER_TLS_ADDR nor BOID_BROKER_SOCKET is set")
	}
}

// TestDialFromEnv_UnixBranch and TestDialFromEnv_TLSBranch pin
// internal/sandbox/shim.go's sendStreamingExecRequest own transport
// selection (ShimExec — host-command exec, not the one-shot JSON path):
// DialFromEnv must apply the exact same priority rule as SendJSONFromEnv,
// just returning the raw connection instead of also doing the JSON
// encode/decode round trip itself.
func TestDialFromEnv_UnixBranch(t *testing.T) {
	b := startFakeBroker(t, 0, "")
	t.Setenv(EnvBrokerTLSAddr, "")
	t.Setenv(EnvBrokerSocket, b.socket)

	conn, err := DialFromEnv()
	if err != nil {
		t.Fatalf("DialFromEnv: %v", err)
	}
	defer conn.Close()
	if _, ok := conn.(*net.UnixConn); !ok {
		t.Errorf("conn type = %T, want *net.UnixConn", conn)
	}
}

func TestDialFromEnv_TLSBranch(t *testing.T) {
	ca, err := mtls.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("mtls.LoadOrCreate: %v", err)
	}
	tlsBroker := startFakeTLSBroker(t, ca, 0, "")
	certPath, keyPath, caPath := writeClientCertFiles(t, ca)

	t.Setenv(EnvBrokerTLSAddr, tlsBroker.addr)
	t.Setenv(EnvBrokerTLSServerName, "127.0.0.1")
	t.Setenv(EnvBrokerTLSCertPath, certPath)
	t.Setenv(EnvBrokerTLSKeyPath, keyPath)
	t.Setenv(EnvBrokerTLSCAPath, caPath)
	t.Setenv(EnvBrokerSocket, "")

	conn, err := DialFromEnv()
	if err != nil {
		t.Fatalf("DialFromEnv: %v", err)
	}
	defer conn.Close()
	if _, ok := conn.(*tls.Conn); !ok {
		t.Errorf("conn type = %T, want *tls.Conn", conn)
	}
}

func TestDialFromEnv_NeitherSet_Errors(t *testing.T) {
	t.Setenv(EnvBrokerTLSAddr, "")
	t.Setenv(EnvBrokerSocket, "")
	if _, err := DialFromEnv(); err == nil {
		t.Fatal("expected an error when neither BOID_BROKER_TLS_ADDR nor BOID_BROKER_SOCKET is set")
	}
}

func TestSendJSON_ConnectError(t *testing.T) {
	err := SendJSON("/nonexistent/socket.sock", map[string]any{"x": 1}, nil)
	if err == nil {
		t.Fatal("expected connect error for missing socket")
	}
}
