package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveSocketPath_UsesArgOrDefault(t *testing.T) {
	t.Setenv("BOID_SOCKET", "/tmp/boid-e2e.sock")

	if got := resolveSocketPath([]string{"/tmp/override.sock"}); got != "/tmp/override.sock" {
		t.Fatalf("resolveSocketPath override = %q", got)
	}
	if got := resolveSocketPath(nil); got != "/tmp/boid-e2e.sock" {
		t.Fatalf("resolveSocketPath default = %q", got)
	}
}

func TestWaitUnixSocket(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "boid.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		conn, err := ln.Accept()
		if err == nil {
			_ = conn.Close()
		}
		errCh <- err
	}()

	if err := waitUnixSocket(ctx, socketPath, 20*time.Millisecond); err != nil {
		t.Fatalf("waitUnixSocket: %v", err)
	}

	if err := <-errCh; err != nil {
		t.Fatalf("accept: %v", err)
	}
}

func TestWaitHealth(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "boid.sock")
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer ln.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	srv := &http.Server{Handler: mux}
	defer srv.Close()
	go srv.Serve(ln)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := waitHealth(ctx, socketPath, 20*time.Millisecond); err != nil {
		t.Fatalf("waitHealth: %v", err)
	}
}

func TestRunGetTask_RequiresTaskID(t *testing.T) {
	if err := runGetTask(nil); err == nil {
		t.Fatal("expected error for missing task id")
	}
}

func TestRunListJobs_RequiresTaskID(t *testing.T) {
	if err := runListJobs(nil); err == nil {
		t.Fatal("expected error for missing task id")
	}
}

func TestPrintJSON(t *testing.T) {
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = oldStdout })

	if err := printJSON(map[string]string{"status": "ok"}); err != nil {
		t.Fatalf("printJSON: %v", err)
	}
	_ = w.Close()

	var got map[string]string
	if err := json.NewDecoder(r).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["status"] != "ok" {
		t.Fatalf("status = %q, want ok", got["status"])
	}
}
