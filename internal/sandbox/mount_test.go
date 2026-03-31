package sandbox_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestBuildMounts(t *testing.T) {
	cfg := sandbox.SandboxConfig{
		ProjectDir:   "/home/user/projects/myproj",
		HooksDir:     "/home/user/projects/myproj/.boid/hooks",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		BrokerSocket: "/run/boid/broker.sock",
		WorkspaceDirs: map[string]string{
			"other-proj": "/home/user/projects/other",
		},
	}

	mounts := sandbox.BuildMounts(cfg)

	// Collect mounts by target for easier assertion
	byTarget := make(map[string]sandbox.BasicMountEntry)
	for _, m := range mounts {
		byTarget[m.Target] = m
	}

	// Project directory (rw) — mounted at host path
	projMount, ok := byTarget[cfg.ProjectDir]
	if !ok {
		t.Fatalf("missing project mount at %s", cfg.ProjectDir)
	}
	if projMount.ReadOnly {
		t.Error("project mount should be rw")
	}
	if projMount.Source != cfg.ProjectDir {
		t.Errorf("project mount source = %q, want %q", projMount.Source, cfg.ProjectDir)
	}

	// Workspace project (ro) — mounted at host path
	wsDir := cfg.WorkspaceDirs["other-proj"]
	wsMount, ok := byTarget[wsDir]
	if !ok {
		t.Fatalf("missing workspace mount at %s", wsDir)
	}
	if !wsMount.ReadOnly {
		t.Error("workspace mount should be ro")
	}

	// .boid directory (ro)
	dotBoidTarget := cfg.ProjectDir + "/.boid"
	dotBoidMount, ok := byTarget[dotBoidTarget]
	if !ok {
		t.Fatalf("missing .boid mount at %s", dotBoidTarget)
	}
	if !dotBoidMount.ReadOnly {
		t.Error(".boid mount should be ro")
	}
	if dotBoidMount.Source != dotBoidTarget {
		t.Errorf(".boid mount source = %q, want %q", dotBoidMount.Source, dotBoidTarget)
	}

	// Boid binary
	boidMount, ok := byTarget["/usr/local/bin/boid"]
	if !ok {
		t.Fatal("missing boid binary mount")
	}
	if !boidMount.ReadOnly {
		t.Error("boid binary mount should be ro")
	}

	// Server socket
	serverMount, ok := byTarget["/run/boid/server.sock"]
	if !ok {
		t.Fatal("missing server socket mount")
	}
	if serverMount.ReadOnly {
		t.Error("server socket mount should be rw")
	}

	// Broker socket
	brokerMount, ok := byTarget["/run/boid/broker.sock"]
	if !ok {
		t.Fatal("missing broker socket mount")
	}
	if brokerMount.ReadOnly {
		t.Error("broker socket mount should be rw")
	}
}

func TestBuildEnv(t *testing.T) {
	cfg := sandbox.SandboxConfig{
		Env: map[string]string{
			"FOO": "bar",
		},
	}

	env := sandbox.BuildEnv(cfg, 8080)

	if env["FOO"] != "bar" {
		t.Errorf("env FOO = %q, want %q", env["FOO"], "bar")
	}

	expectedProxy := "http://10.0.2.2:8080"
	if env["http_proxy"] != expectedProxy {
		t.Errorf("http_proxy = %q, want %q", env["http_proxy"], expectedProxy)
	}
	if env["https_proxy"] != expectedProxy {
		t.Errorf("https_proxy = %q, want %q", env["https_proxy"], expectedProxy)
	}
	if env["HTTP_PROXY"] != expectedProxy {
		t.Errorf("HTTP_PROXY = %q, want %q", env["HTTP_PROXY"], expectedProxy)
	}
	if env["HTTPS_PROXY"] != expectedProxy {
		t.Errorf("HTTPS_PROXY = %q, want %q", env["HTTPS_PROXY"], expectedProxy)
	}
	if env["BOID_SOCKET"] != "/run/boid/server.sock" {
		t.Errorf("BOID_SOCKET = %q, want %q", env["BOID_SOCKET"], "/run/boid/server.sock")
	}
	if env["BOID_BROKER_SOCKET"] != "/run/boid/broker.sock" {
		t.Errorf("BOID_BROKER_SOCKET = %q, want %q", env["BOID_BROKER_SOCKET"], "/run/boid/broker.sock")
	}

	// Test without proxy
	envNoProxy := sandbox.BuildEnv(cfg, 0)
	if _, ok := envNoProxy["http_proxy"]; ok {
		t.Error("expected no http_proxy when proxyPort is 0")
	}
}

func TestBuildEnv_NoProxy(t *testing.T) {
	cfg := sandbox.SandboxConfig{
		Env: map[string]string{"KEY": "val"},
	}

	env := sandbox.BuildEnv(cfg, 0)
	if env["KEY"] != "val" {
		t.Errorf("env KEY = %q, want %q", env["KEY"], "val")
	}
	if _, ok := env["http_proxy"]; ok {
		t.Error("expected no http_proxy when proxyPort is 0")
	}
}

func TestShimLinks(t *testing.T) {
	links := sandbox.ShimLinks([]string{"git", "gh", "npm"})

	expected := map[string]string{
		"/usr/bin/git": "/usr/local/bin/boid",
		"/usr/bin/gh":  "/usr/local/bin/boid",
		"/usr/bin/npm": "/usr/local/bin/boid",
	}

	if len(links) != len(expected) {
		t.Fatalf("len(links) = %d, want %d", len(links), len(expected))
	}

	for k, v := range expected {
		if links[k] != v {
			t.Errorf("links[%q] = %q, want %q", k, links[k], v)
		}
	}
}
