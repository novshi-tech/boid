package sandbox

import (
	"fmt"
	"path/filepath"
)

// BasicMountEntry describes a bind mount for the sandbox.
type BasicMountEntry struct {
	Source   string
	Target   string
	ReadOnly bool
}

// BuildMounts generates the mount entries for a sandbox configuration.
func BuildMounts(cfg SandboxConfig) []BasicMountEntry {
	var mounts []BasicMountEntry

	// Project directory (rw) — mounted at same path as host
	mounts = append(mounts, BasicMountEntry{
		Source:   cfg.ProjectDir,
		Target:   cfg.ProjectDir,
		ReadOnly: false,
	})

	// Workspace projects (ro) — mounted at host paths
	for _, dir := range cfg.WorkspaceDirs {
		mounts = append(mounts, BasicMountEntry{
			Source:   dir,
			Target:   dir,
			ReadOnly: true,
		})
	}

	// .boid directory (ro) — project config not writable from sandbox
	mounts = append(mounts, BasicMountEntry{
		Source:   filepath.Join(cfg.ProjectDir, ".boid"),
		Target:   filepath.Join(cfg.ProjectDir, ".boid"),
		ReadOnly: true,
	})

	// Boid binary
	if cfg.BoidBinary != "" {
		mounts = append(mounts, BasicMountEntry{
			Source:   cfg.BoidBinary,
			Target:   "/usr/local/bin/boid",
			ReadOnly: true,
		})
	}

	// Broker socket
	if cfg.BrokerSocket != "" {
		mounts = append(mounts, BasicMountEntry{
			Source:   cfg.BrokerSocket,
			Target:   "/run/boid/broker.sock",
			ReadOnly: false,
		})
	}

	// Server socket
	if cfg.ServerSocket != "" {
		mounts = append(mounts, BasicMountEntry{
			Source:   cfg.ServerSocket,
			Target:   "/run/boid/server.sock",
			ReadOnly: false,
		})
	}

	// Additional bindings
	for _, b := range cfg.Bindings {
		mounts = append(mounts, BasicMountEntry{
			Source:   b,
			Target:   b,
			ReadOnly: true,
		})
	}

	return mounts
}

// BuildEnv generates the environment variables for a sandbox.
func BuildEnv(cfg SandboxConfig, proxyPort int) map[string]string {
	env := make(map[string]string)

	// Copy project env
	for k, v := range cfg.Env {
		env[k] = v
	}

	// Proxy settings
	if proxyPort > 0 {
		proxyURL := fmt.Sprintf("http://10.0.2.2:%d", proxyPort)
		env["http_proxy"] = proxyURL
		env["https_proxy"] = proxyURL
		env["HTTP_PROXY"] = proxyURL
		env["HTTPS_PROXY"] = proxyURL
	}

	// Boid socket
	env["BOID_SOCKET"] = "/run/boid/server.sock"
	env["BOID_BROKER_SOCKET"] = "/run/boid/broker.sock"

	return env
}

// ShimLinks returns the symlinks to create for host commands.
func ShimLinks(commands []string) map[string]string {
	links := make(map[string]string)
	for _, cmd := range commands {
		links[fmt.Sprintf("/usr/bin/%s", cmd)] = "/usr/local/bin/boid"
	}
	return links
}
