package tunnel

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func workspaceHandler(ws string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(ws)) })
}

func askWorkspace(t *testing.T, conn *websocket.Conn) string {
	t.Helper()
	req, _ := json.Marshal(map[string]any{"type": "request", "id": "r1", "method": "GET", "path": "/projects", "headers": [][2]string{}})
	if err := conn.Write(context.Background(), websocket.MessageText, req); err != nil {
		t.Fatal(err)
	}
	return decodeBody(t, readFrame(t, conn).Body)
}

func TestSetPairsEachWorkspaceOnItsOwn(t *testing.T) {
	nvt, khi := newFakeGateway(t), newFakeGateway(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	dir := filepath.Join(t.TempDir(), "tunnels")
	s := &Set{
		Dir:         dir,
		Handler:     workspaceHandler,
		Description: func(ws string) string { return "boid for " + ws },
		CheckWorkspace: func(ws string) error {
			if ws == "missing" {
				return errors.New(`workspace "missing" not found`)
			}
			return nil
		},
	}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Pair("default", nvt.srv.URL, ""); err != nil {
		t.Fatal(err)
	}
	nvtConn := nvt.nextConn(t)
	<-nvt.hellos
	if _, err := s.Pair("khi", khi.srv.URL, ""); err != nil {
		t.Fatal(err)
	}
	khiConn := khi.nextConn(t)
	<-khi.hellos
	for _, ws := range []string{"default", "khi"} {
		if d := s.managers[ws].Description; d != "boid for "+ws {
			t.Fatalf("%s description = %q", ws, d)
		}
	}

	if got := askWorkspace(t, nvtConn); got != "default" {
		t.Fatalf("nvt gateway reached %q", got)
	}
	if got := askWorkspace(t, khiConn); got != "khi" {
		t.Fatalf("khi gateway reached %q", got)
	}
	if s.Caller("khi") == nil || s.Caller("default") == nil || s.Caller("other") != nil {
		t.Fatal("callers are not per workspace")
	}
	for _, ws := range []string{"default", "khi"} {
		if _, err := os.Stat(filepath.Join(dir, ws+".json")); err != nil {
			t.Fatalf("%s identity: %v", ws, err)
		}
	}
	if st := s.Status(); len(st) != 2 || st[0].Workspace != "default" || st[1].Workspace != "khi" {
		t.Fatalf("status = %+v", st)
	}

	if _, err := s.Pair("missing", khi.srv.URL, ""); err == nil {
		t.Fatal("paired a workspace that does not exist")
	}
	if _, err := s.Pair("../x", khi.srv.URL, ""); err == nil {
		t.Fatal("paired a path as a workspace")
	}

	if err := s.Forget("khi"); err != nil {
		t.Fatal(err)
	}
	if s.Caller("khi") != nil {
		t.Fatal("khi still connected after forget")
	}
	if _, err := os.Stat(filepath.Join(dir, "khi.json")); !os.IsNotExist(err) {
		t.Fatalf("khi identity kept: %v", err)
	}
	if s.Caller("default") == nil {
		t.Fatal("forgetting khi dropped default")
	}
}

func TestSetMovesTheOldIdentityToTheDefaultWorkspace(t *testing.T) {
	g := newFakeGateway(t)
	home := t.TempDir()
	priv, _ := GenerateKey()
	g.publicKey = priv.Public().(ed25519.PublicKey)
	legacy := filepath.Join(home, "tunnel.json")
	if err := SaveIdentity(legacy, &Identity{Gateway: g.srv.URL, ServiceID: "svc1", Name: "boid", PrivateKey: priv}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Set{Dir: filepath.Join(home, "tunnels"), LegacyPath: legacy, Handler: workspaceHandler}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	conn := g.nextConn(t)
	<-g.hellos
	if got := askWorkspace(t, conn); got != DefaultWorkspace {
		t.Fatalf("old identity reached %q", got)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("old identity still at %s: %v", legacy, err)
	}
}

func TestSetManagementAPI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := &Set{Dir: t.TempDir()}
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body string
		want               int
	}{
		{"GET", MountPath, "", http.StatusOK},
		{"GET", MountPath + "/khi", "", http.StatusOK},
		{"POST", MountPath + "/khi/pair", `{"gateway":"http://example.com"}`, http.StatusBadRequest},
		{"DELETE", MountPath + "/khi", "", http.StatusNoContent},
		{"POST", MountPath + "/pair", `{"gateway":"https://example.com"}`, http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d (%s), want %d", tc.method, tc.path, rec.Code, rec.Body.String(), tc.want)
		}
	}
}
