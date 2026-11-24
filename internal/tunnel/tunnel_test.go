package tunnel

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeGateway implements the gateway side of docs/tunnel-protocol.md.
type fakeGateway struct {
	t   *testing.T
	srv *httptest.Server

	mu         sync.Mutex
	publicKey  ed25519.PublicKey
	challenges map[string]bool
	conns      chan *websocket.Conn
	hellos     chan inFrame
}

func newFakeGateway(t *testing.T) *fakeGateway {
	g := &fakeGateway{t: t, challenges: map[string]bool{}, conns: make(chan *websocket.Conn, 4), hellos: make(chan inFrame, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tunnel/pair", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Name      string `json:"name"`
			PublicKey jwk    `json:"public_key"`
			Mode      string `json:"mode"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.PublicKey.D != "" || in.Mode != "standard" || in.Name == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		x, _ := b64.DecodeString(in.PublicKey.X)
		g.mu.Lock()
		g.publicKey = x
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "ABCD-EFGH", "pairing_id": "p1", "verification_uri_complete": g.srv.URL + "/pair?code=ABCD-EFGH",
			"expires_in": 300, "interval": 1,
		})
	})
	mux.HandleFunc("POST /tunnel/pair/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "approved", "service_id": "svc1", "name": "boid"})
	})
	mux.HandleFunc("POST /tunnel/challenge", func(w http.ResponseWriter, r *http.Request) {
		c := newID()
		g.mu.Lock()
		g.challenges[c] = true
		g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"challenge": c, "expires_in": 60})
	})
	mux.HandleFunc("GET /tunnel/connect", func(w http.ResponseWriter, r *http.Request) {
		if !g.verify(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		conn.SetReadLimit(maxFrameBytes)
		ctx := context.Background()
		_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"welcome","service_id":"svc1","name":"boid","max_body_bytes":8388608,"timeout_ms":30000}`))
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var hello inFrame
		_ = json.Unmarshal(data, &hello)
		g.hellos <- hello
		g.conns <- conn
		<-r.Context().Done()
	})
	g.srv = httptest.NewServer(mux)
	t.Cleanup(g.srv.Close)
	return g
}

func (g *fakeGateway) verify(assertion string) bool {
	parts := strings.Split(assertion, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil {
		return false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.publicKey == nil || !ed25519.Verify(g.publicKey, []byte(parts[0]+"."+parts[1]), sig) {
		return false
	}
	raw, _ := b64.DecodeString(parts[1])
	var claims struct {
		Iss   string `json:"iss"`
		Aud   string `json:"aud"`
		Nonce string `json:"nonce"`
		Exp   int64  `json:"exp"`
	}
	_ = json.Unmarshal(raw, &claims)
	if claims.Iss != "svc1" || claims.Aud != g.srv.URL+"/tunnel/connect" || !g.challenges[claims.Nonce] || claims.Exp < time.Now().Unix() {
		return false
	}
	delete(g.challenges, claims.Nonce)
	return true
}

func (g *fakeGateway) nextConn(t *testing.T) *websocket.Conn {
	t.Helper()
	select {
	case c := <-g.conns:
		return c
	case <-time.After(10 * time.Second):
		t.Fatal("no tunnel connection")
		return nil
	}
}

func readFrame(t *testing.T, conn *websocket.Conn) inFrame {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if string(data) == "ping" {
			continue
		}
		var f inFrame
		if err := json.Unmarshal(data, &f); err != nil {
			t.Fatalf("frame %q: %v", data, err)
		}
		return f
	}
}

func decodeBody(t *testing.T, s *string) string {
	t.Helper()
	if s == nil {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(*s)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func startPairedManager(t *testing.T, g *fakeGateway, handler http.Handler) *Manager {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	m := &Manager{IdentityPath: filepath.Join(t.TempDir(), "tunnel.json"), Handler: handler, Description: "test boid"}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	p, err := m.Pair(g.srv.URL, "boid")
	if err != nil {
		t.Fatal(err)
	}
	if p.Code != "ABCD-EFGH" || p.Thumbprint == "" {
		t.Fatalf("pairing = %+v", p)
	}
	return m
}

func TestPairConnectAndServeOwnerRequests(t *testing.T) {
	g := newFakeGateway(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "x=y")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"method": r.Method, "path": r.URL.RequestURI(), "body": string(body), "accept": r.Header.Get("Accept")})
	})
	m := startPairedManager(t, g, handler)
	conn := g.nextConn(t)

	hello := <-g.hellos
	if hello.Type != "hello" {
		t.Fatalf("first frame = %+v, want hello", hello)
	}

	info, err := os.Stat(m.IdentityPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity file mode = %v, want 0600", info.Mode().Perm())
	}
	if _, err := LoadIdentity(m.IdentityPath); err != nil {
		t.Fatal(err)
	}

	body := base64.StdEncoding.EncodeToString([]byte(`{"a":1}`))
	req, _ := json.Marshal(map[string]any{"type": "request", "id": "r1", "method": "POST", "path": "/jobs?status=running", "headers": [][2]string{{"accept", "application/json"}}, "body": body})
	if err := conn.Write(context.Background(), websocket.MessageText, req); err != nil {
		t.Fatal(err)
	}
	resp := readFrame(t, conn)
	if resp.Type != "response" || resp.ID != "r1" || resp.Status != http.StatusAccepted {
		t.Fatalf("response = %+v", resp)
	}
	var got map[string]string
	_ = json.Unmarshal([]byte(decodeBody(t, resp.Body)), &got)
	if got["method"] != "POST" || got["path"] != "/jobs?status=running" || got["body"] != `{"a":1}` || got["accept"] != "application/json" {
		t.Fatalf("handler saw %v", got)
	}
	for _, h := range resp.Headers {
		if h[0] == "set-cookie" {
			t.Fatalf("set-cookie leaked: %v", resp.Headers)
		}
	}
	if s := m.Status(); !s.Paired || s.Connection == nil || !s.Connection.Connected || s.Connection.ServiceID != "svc1" {
		t.Fatalf("status = %+v", s)
	}
}

func TestRelayCallsBackAsOwner(t *testing.T) {
	g := newFakeGateway(t)
	m := startPairedManager(t, g, http.NotFoundHandler())
	conn := g.nextConn(t)
	<-g.hellos

	relay := &Relay{
		Lookup: func(token string) (JobGrant, bool) {
			return JobGrant{WorkspaceID: "dev", JobRef: "task-1"}, token == "tok"
		},
		AllowWorkspace: func(ws string) bool { return ws == "dev" },
		Caller:         func(string) Caller { return m.Caller() },
	}

	done := make(chan *httptest.ResponseRecorder)
	go func() {
		r := httptest.NewRequest(http.MethodPost, "/heddleway/tok/freee/api/1/deals?company_id=1", strings.NewReader(`{"x":1}`))
		r.Header.Set("Authorization", "Bearer leaked")
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		relay.ServeHTTP(rec, r)
		done <- rec
	}()

	call := readFrame(t, conn)
	if call.Type != "call" || call.Method != "POST" || call.Path != "/api/1/deals?company_id=1" || decodeBody(t, call.Body) != `{"x":1}` {
		t.Fatalf("call = %+v", call)
	}
	var sawJob bool
	for _, h := range call.Headers {
		if h[0] == "authorization" {
			t.Fatalf("authorization forwarded: %v", call.Headers)
		}
		if h[0] == "x-boid-job" && h[1] == "task-1" {
			sawJob = true
		}
	}
	if !sawJob {
		t.Fatalf("x-boid-job missing: %v", call.Headers)
	}
	result, _ := json.Marshal(map[string]any{"type": "call_result", "id": call.ID, "status": 200, "headers": [][2]string{{"content-type", "application/json"}}, "body": base64.StdEncoding.EncodeToString([]byte(`{"ok":true}`))})
	if err := conn.Write(context.Background(), websocket.MessageText, result); err != nil {
		t.Fatal(err)
	}
	rec := <-done
	if rec.Code != http.StatusOK || rec.Body.String() != `{"ok":true}` || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("relay answered %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type stubCaller struct{ called bool }

func (s *stubCaller) Call(context.Context, string, string, string, [][2]string, []byte) (*CallResult, error) {
	s.called = true
	return &CallResult{Status: http.StatusOK}, nil
}

func TestRelayRefusals(t *testing.T) {
	grants := map[string]JobGrant{
		"dev":      {WorkspaceID: "dev"},
		"customer": {WorkspaceID: "customer"},
		"readonly": {WorkspaceID: "dev", ReadOnly: true},
	}
	caller := &stubCaller{}
	var connected Caller = caller
	relay := &Relay{
		Lookup:         func(token string) (JobGrant, bool) { g, ok := grants[token]; return g, ok },
		AllowWorkspace: func(ws string) bool { return ws == "dev" },
		Caller:         func(string) Caller { return connected },
	}
	cases := []struct {
		name, method, path string
		origin             bool
		want               int
	}{
		{"unknown token", "GET", "/heddleway/nope/freee/x", false, http.StatusUnauthorized},
		{"workspace not allowed", "GET", "/heddleway/customer/freee/x", false, http.StatusForbidden},
		{"read-only write", "POST", "/heddleway/readonly/freee/x", false, http.StatusForbidden},
		{"read-only read", "GET", "/heddleway/readonly/freee/x", false, http.StatusOK},
		{"browser", "GET", "/heddleway/dev/freee/x", true, http.StatusForbidden},
		{"bad service", "GET", "/heddleway/dev/Free!/x", false, http.StatusNotFound},
		{"allowed", "DELETE", "/heddleway/dev/freee/x", false, http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			caller.called = false
			r := httptest.NewRequest(tc.method, tc.path, nil)
			if tc.origin {
				r.Header.Set("Origin", "https://evil.example")
			}
			rec := httptest.NewRecorder()
			relay.ServeHTTP(rec, r)
			if rec.Code != tc.want {
				t.Fatalf("status = %d (%s), want %d", rec.Code, rec.Body.String(), tc.want)
			}
			if caller.called != (tc.want == http.StatusOK) {
				t.Fatalf("caller.called = %v", caller.called)
			}
		})
	}

	connected = nil
	rec := httptest.NewRecorder()
	relay.ServeHTTP(rec, httptest.NewRequest("GET", "/heddleway/dev/freee/x", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unpaired status = %d, want 503", rec.Code)
	}
}

func TestClientStopsWhenRegistrationRemoved(t *testing.T) {
	g := newFakeGateway(t)
	key, _ := GenerateKey()
	g.publicKey = key.Public().(ed25519.PublicKey)
	c := &Client{Identity: &Identity{Gateway: g.srv.URL, ServiceID: "svc1", Name: "boid", PrivateKey: key}, Handler: http.NotFoundHandler()}
	errc := make(chan error, 1)
	go func() { errc <- c.Run(context.Background()) }()
	conn := g.nextConn(t)
	<-g.hellos
	_ = conn.Close(closeRemoved, "removed")
	select {
	case err := <-errc:
		if websocket.CloseStatus(err) != closeRemoved {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("client kept running after 4003")
	}
	if s := c.Status(); s.Stopped == "" || s.Connected {
		t.Fatalf("status = %+v", s)
	}
}

func TestClientReconnectsAfterDrop(t *testing.T) {
	g := newFakeGateway(t)
	key, _ := GenerateKey()
	g.publicKey = key.Public().(ed25519.PublicKey)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &Client{Identity: &Identity{Gateway: g.srv.URL, ServiceID: "svc1", Name: "boid", PrivateKey: key}, Handler: http.NotFoundHandler()}
	go func() { _ = c.Run(ctx) }()
	conn := g.nextConn(t)
	<-g.hellos
	_ = conn.Close(websocket.StatusGoingAway, "deploy")
	g.nextConn(t)
	<-g.hellos
}

func TestLoadIdentityReadsTunnelClientFormat(t *testing.T) {
	key, _ := GenerateKey()
	pub := publicJWK(key)
	path := filepath.Join(t.TempDir(), "key.json")
	file := map[string]any{
		"gateway": "https://gateway.example/", "service_id": "svc1", "name": "boid",
		"private_jwk": map[string]any{"kty": "OKP", "crv": "Ed25519", "x": pub.X, "d": b64.EncodeToString(key.Seed()), "key_ops": []string{"sign"}, "ext": true},
	}
	if err := os.WriteFile(path, mustJSON(t, file), 0o600); err != nil {
		t.Fatal(err)
	}
	id, err := LoadIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if id.Gateway != "https://gateway.example" || !id.PrivateKey.Equal(key) {
		t.Fatalf("identity = %+v", id)
	}
}
