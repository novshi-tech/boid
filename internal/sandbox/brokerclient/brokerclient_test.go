package brokerclient

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
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

func TestJobDone_WireFormat(t *testing.T) {
	b := startFakeBroker(t, 0, "")

	err := JobDone(b.socket, "tok-123", "job-7", "/work/dir", 0, []byte(`{"artifact":{}}`))
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
	if err := JobDone(b.socket, "t", "j", "/w", 42, nil); err != nil {
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
	err := JobDone(b.socket, "t", "j", "/w", 0, nil)
	if err == nil {
		t.Fatal("expected error when broker rejects job done")
	}
}

func TestSendJSON_ConnectError(t *testing.T) {
	err := SendJSON("/nonexistent/socket.sock", map[string]any{"x": 1}, nil)
	if err == nil {
		t.Fatal("expected connect error for missing socket")
	}
}
