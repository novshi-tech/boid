package server_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/server"
)

func TestServer_StartAndStop(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "boid.sock")

	cfg := server.Config{
		DBPath:         ":memory:",
		SocketPath:     sockPath,
		HTTPAddr:       "127.0.0.1:0",
		AllowedDomains: []string{"example.com"},
		WebEnabled:     true,
	}

	srv, err := server.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := srv.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Health check via UNIX socket
	httpClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			},
		},
	}

	resp, err := httpClient.Get("http://boid/api/health")
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("health status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var health map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil {
		t.Fatalf("decode health: %v", err)
	}
	if health["status"] != "ok" {
		t.Errorf("health status = %q, want %q", health["status"], "ok")
	}

	// Proxy API check
	proxyResp, err := httpClient.Get("http://boid/api/proxy")
	if err != nil {
		t.Fatalf("proxy check: %v", err)
	}
	defer proxyResp.Body.Close()

	var proxyInfo struct{ Port int }
	if err := json.NewDecoder(proxyResp.Body).Decode(&proxyInfo); err != nil {
		t.Fatalf("decode proxy: %v", err)
	}
	if proxyInfo.Port == 0 {
		t.Error("expected non-zero proxy port")
	}
	if srv.ProxyPort() != proxyInfo.Port {
		t.Errorf("ProxyPort() = %d, api returned %d", srv.ProxyPort(), proxyInfo.Port)
	}

	// Health check via TCP
	tcpAddr := srv.TCPAddr()
	if tcpAddr == "" {
		t.Fatal("expected non-empty TCP address")
	}

	tcpResp, err := http.Get("http://" + tcpAddr + "/api/health")
	if err != nil {
		t.Fatalf("tcp health check: %v", err)
	}
	defer tcpResp.Body.Close()

	if tcpResp.StatusCode != http.StatusOK {
		t.Errorf("tcp health status = %d, want %d", tcpResp.StatusCode, http.StatusOK)
	}

	// Broker socket check
	brokerSock := srv.BrokerSocket()
	if brokerSock == "" {
		t.Fatal("expected non-empty broker socket path")
	}

	// Stop
	if err := srv.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Verify socket is cleaned up
	_, err = net.Dial("unix", sockPath)
	if err == nil {
		t.Error("expected error connecting to stopped server")
	}

	// Verify broker socket is cleaned up
	_, err = net.Dial("unix", brokerSock)
	if err == nil {
		t.Error("expected error connecting to stopped broker")
	}
}
