package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/coder/websocket"
	"github.com/novshi-tech/boid/internal/api/auth"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/server"
)

// TestTCPListener_WSAttach_ReachableViaBearerAndCookie is the regression
// test for Phase 3 PR3's wire.go change (docs/plans/cli-remote-connection.md
// "WebSocket attach 一本化" / PR3: "サーバ側 WS route の Bearer 完全結線"):
// GET /api/jobs/{id}/attach/ws used to be mounted inside the cookie-only
// WebAuthMiddleware Group, so a Bearer-only caller (no session cookie) could
// not reach it over TCP even with a perfectly valid device token — the
// handshake would be rejected before WSAttachHandler.authenticateDevice ever
// got a chance to check the Authorization header. PR3 moved the route
// mount point out of that Group so the same TCPAPIAuthMiddleware wrapping
// every other /api/* route gates this one too. This test dials the route
// three ways over the real TCP listener: a valid Bearer token (must
// succeed), a valid session cookie (must succeed — the pre-existing Web UI
// path must keep working), and neither (must be rejected — the loopback
// bootstrap window closes once a device is registered, matching
// TestTCPListener_DataAPI_RequiresAuth's own case 4 vs. its tunneled case).
//
// The target job id does not exist, which is fine: WSAttachHandler's
// Subscriber.Subscribe (backed by runtime.runner, wired unconditionally in
// buildRuntime) just reports ok=false for an unknown id, so the handshake
// still completes (101) and the handler immediately sends "exit" and
// closes — this test only cares that the handshake itself is reachable,
// not about a real job's output.
func TestTCPListener_WSAttach_ReachableViaBearerAndCookie(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	srv, err := server.New(server.Config{
		DBPath:     ":memory:",
		SocketPath: sockPath,
		HTTPAddr:   "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer srv.Stop()

	tcpAddr := srv.TCPAddr()
	if tcpAddr == "" {
		t.Fatal("TCP listener should be open")
	}

	store := auth.NewStore(srv.DB())

	// --- Bearer path ---
	token, err := auth.GenerateDeviceToken()
	if err != nil {
		t.Fatalf("GenerateDeviceToken: %v", err)
	}
	if err := store.InsertDeviceToken(context.Background(), "dev-wire-bearer", "cli", auth.HashToken(token)); err != nil {
		t.Fatalf("InsertDeviceToken: %v", err)
	}

	wsURL := "ws://" + tcpAddr + "/api/jobs/nonexistent-job/attach/ws"

	bearerConn, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": []string{"Bearer " + token}},
	})
	if err != nil {
		t.Fatalf("bearer ws dial: %v", err)
	}
	bearerConn.CloseNow()

	// --- Cookie path (mirrors a Web UI browser's session) ---
	// webSecretPathFor(cfg) — internal/server, unexported — resolves to
	// filepath.Join(filepath.Dir(cfg.SocketPath), "web_secret") whenever
	// DBPath is ":memory:" (this test's config), and buildRuntime already
	// created that file via dispatcher.LoadOrCreateKey during srv.Start()
	// above. Reading it back with the same helper is idempotent (it only
	// creates the file if missing) and hands back the exact same secret
	// bytes the running daemon's SessionSigner holds, so a cookie minted
	// here verifies as genuine.
	secret, err := dispatcher.LoadOrCreateKey(filepath.Join(tmpDir, "web_secret"))
	if err != nil {
		t.Fatalf("LoadOrCreateKey: %v", err)
	}
	if err := store.InsertDevice(context.Background(), "dev-wire-cookie", "browser", []byte("h")); err != nil {
		t.Fatalf("InsertDevice: %v", err)
	}
	signer := auth.NewSessionSigner(secret, store)
	rec := httptest.NewRecorder()
	if err := signer.Issue(rec, "dev-wire-cookie"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("expected exactly 1 Set-Cookie, got %d", len(cookies))
	}

	cookieConn, _, err := websocket.Dial(context.Background(), wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Cookie": []string{cookies[0].String()}},
	})
	if err != nil {
		t.Fatalf("cookie ws dial: %v", err)
	}
	cookieConn.CloseNow()

	// --- No auth at all: once a device is registered, the loopback
	// bootstrap window is closed (matches TestTCPListener_DataAPI_
	// RequiresAuth's genuine-browser-before-pairing case, but here devices
	// already exist) so the handshake must be rejected before any WS
	// upgrade happens.
	_, resp, err := websocket.Dial(context.Background(), wsURL, nil)
	if err == nil {
		t.Fatal("expected unauthenticated ws dial to fail once a device is registered")
	}
	if resp != nil && resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}
