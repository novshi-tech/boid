package job_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/job"
	"github.com/novshi-tech/boid/internal/model"
)

func TestWriteSandboxScripts(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-job-001",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		BrokerSocket: "/run/boid/broker.sock",
		BrokerToken:  "test-token-abc",
		Env: map[string]string{
			"MY_VAR": "hello",
		},
		HostCommands: []string{"git", "gh"},
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-job-001"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"

	// Clean up generated scripts after test
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	// Verify outer script
	outerContent, err := os.ReadFile(outerPath)
	if err != nil {
		t.Fatalf("read outer script: %v", err)
	}
	outer := string(outerContent)
	if !strings.Contains(outer, "pasta --config-net") {
		t.Error("outer script missing 'pasta --config-net'")
	}
	if !strings.Contains(outer, "unshare --mount") {
		t.Error("outer script missing 'unshare --mount'")
	}
	if !strings.Contains(outer, "2>/dev/null") {
		t.Error("outer script should suppress stderr in job mode")
	}

	// Verify setup script
	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)
	if !strings.Contains(setup, "unshare --user") {
		t.Error("setup script missing 'unshare --user'")
	}
	if !strings.Contains(setup, cfg.ProjectDir) {
		t.Errorf("setup script missing project dir %q", cfg.ProjectDir)
	}
	// Verify .boid directory is mounted read-only
	if !strings.Contains(setup, fmt.Sprintf("mount --bind %s/.boid \"$ROOT%s/.boid\"", cfg.ProjectDir, cfg.ProjectDir)) {
		t.Error("setup script missing .boid bind mount")
	}
	if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s/.boid\"", cfg.ProjectDir)) {
		t.Error("setup script missing .boid read-only remount")
	}
	// Verify hooks overlay in hook mode
	if !strings.Contains(setup, fmt.Sprintf("mount --bind %s \"$ROOT%s/.boid/hooks\"", cfg.HooksDir, cfg.ProjectDir)) {
		t.Error("setup script missing hooks overlay bind mount")
	}
	if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s/.boid/hooks\"", cfg.ProjectDir)) {
		t.Error("setup script missing hooks overlay read-only remount")
	}

	// Verify shim symlinks for host commands
	if !strings.Contains(setup, `ln -sf boid "$ROOT/opt/boid/bin/git"`) {
		t.Error("setup script missing git shim symlink")
	}
	if !strings.Contains(setup, `ln -sf boid "$ROOT/opt/boid/bin/gh"`) {
		t.Error("setup script missing gh shim symlink")
	}

	// Verify inner script
	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	inner := string(innerContent)
	if !strings.Contains(inner, "boid job done test-job-001") {
		t.Error("inner script missing 'boid job done {jobID}'")
	}
	expectedHookPath := cfg.ProjectDir + "/.boid/hooks/run-agent.sh"
	if !strings.Contains(inner, expectedHookPath) {
		t.Errorf("inner script missing hook invocation at %s", expectedHookPath)
	}
	if strings.Contains(inner, "exec "+expectedHookPath) {
		t.Error("inner script must not use exec (would skip EXIT trap)")
	}
	if !strings.Contains(inner, "BOID_SOCKET=/run/boid/server.sock") {
		t.Error("inner script missing BOID_SOCKET")
	}
	if !strings.Contains(inner, `MY_VAR="hello"`) {
		t.Error("inner script missing env var MY_VAR")
	}
	if !strings.Contains(inner, "BOID_BROKER_SOCKET=/run/boid/broker.sock") {
		t.Error("inner script missing BOID_BROKER_SOCKET")
	}
	if !strings.Contains(inner, `BOID_BROKER_TOKEN=test-token-abc`) {
		t.Error("inner script missing BOID_BROKER_TOKEN")
	}

	// Setup script: verify broker socket mount
	if !strings.Contains(setup, `mount --bind /run/boid/broker.sock "$ROOT/run/boid/broker.sock"`) {
		t.Error("setup script missing broker socket bind mount")
	}
}

func TestWriteSandboxScripts_TTY(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-tty-001",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		Env: map[string]string{
			"MY_VAR": "hello",
		},
		HostCommands: []string{"git"},
		Command:      "/bin/bash",
		TTY:          true,
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-tty-001"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	// Outer script: TTY mode should save/restore stderr
	outerContent, err := os.ReadFile(outerPath)
	if err != nil {
		t.Fatalf("read outer script: %v", err)
	}
	outer := string(outerContent)
	if !strings.Contains(outer, "exec 3>&2") {
		t.Error("outer script missing fd save (exec 3>&2)")
	}
	if !strings.Contains(outer, "exec 2>&3 3>&-") {
		t.Error("outer script missing fd restore (exec 2>&3 3>&-)")
	}

	// Inner script: should exec the command
	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	inner := string(innerContent)

	if !strings.Contains(inner, "exec /bin/bash") {
		t.Error("inner script missing 'exec /bin/bash'")
	}
	if strings.Contains(inner, "boid job done") {
		t.Error("inner script must not contain 'boid job done' in command mode")
	}
	if !strings.Contains(inner, `MY_VAR="hello"`) {
		t.Error("inner script missing env var MY_VAR")
	}

	// Setup script
	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)

	// .boid directory should be mounted read-only even in command mode
	if !strings.Contains(setup, fmt.Sprintf("mount --bind %s/.boid \"$ROOT%s/.boid\"", cfg.ProjectDir, cfg.ProjectDir)) {
		t.Error("setup script missing .boid bind mount in command mode")
	}
	if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s/.boid\"", cfg.ProjectDir)) {
		t.Error("setup script missing .boid read-only remount in command mode")
	}
	// Should not mount hooks directory in command mode
	if strings.Contains(setup, ".boid/hooks") {
		t.Error("setup script must not mount hooks directory in command mode")
	}
	if !strings.Contains(setup, "unshare --user") {
		t.Error("setup script missing 'unshare --user'")
	}
}

func TestWriteSandboxScripts_Proxy(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-job-proxy",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		ProxyPort:    8888,
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-job-proxy"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	// Setup script: should contain nftables rules
	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)

	if !strings.Contains(setup, "nft add table inet filter") {
		t.Error("setup script missing nftables table creation")
	}
	if !strings.Contains(setup, "policy drop") {
		t.Error("setup script missing nftables DROP policy")
	}
	if !strings.Contains(setup, "ip daddr 10.0.2.2 accept") {
		t.Error("setup script missing host localhost allow rule")
	}
	if !strings.Contains(setup, "ip daddr 10.0.2.3 accept") {
		t.Error("setup script missing DNS allow rule")
	}

	// Inner script: should contain proxy environment variables
	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	inner := string(innerContent)

	if !strings.Contains(inner, "https_proxy=http://10.0.2.2:8888") {
		t.Error("inner script missing https_proxy")
	}
	if !strings.Contains(inner, "http_proxy=http://10.0.2.2:8888") {
		t.Error("inner script missing http_proxy")
	}
	if !strings.Contains(inner, "no_proxy=10.0.2.2,10.0.2.3,localhost,127.0.0.1") {
		t.Error("inner script missing no_proxy")
	}
}

func TestWriteSandboxScripts_NoProxy(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-job-noproxy",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		ProxyPort:    0,
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-job-noproxy"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	// Setup script: should NOT contain nftables rules
	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	if strings.Contains(string(setupContent), "nft ") {
		t.Error("setup script should not contain nftables when ProxyPort is 0")
	}

	// Inner script: should NOT contain proxy env
	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	if strings.Contains(string(innerContent), "http_proxy") {
		t.Error("inner script should not contain proxy env when ProxyPort is 0")
	}
}

func TestWriteSandboxScripts_WorkspaceDirs(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-job-ws",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		WorkspaceDirs: map[string]string{
			"proj-2": "/home/user/projects/proj-2",
			"proj-3": "/home/user/projects/proj-3",
		},
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-job-ws"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)

	// Verify project mounted at host path (rw)
	if !strings.Contains(setup, fmt.Sprintf("mount --bind %s \"$ROOT%s\"", cfg.ProjectDir, cfg.ProjectDir)) {
		t.Errorf("setup script missing project mount at host path %s", cfg.ProjectDir)
	}

	// Verify workspace peers mounted at host paths (ro)
	for _, dir := range cfg.WorkspaceDirs {
		if !strings.Contains(setup, fmt.Sprintf("mount --bind %s \"$ROOT%s\"", dir, dir)) {
			t.Errorf("setup script missing workspace mount for %s", dir)
		}
		if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s\"", dir)) {
			t.Errorf("setup script missing read-only remount for workspace dir %s", dir)
		}
	}
}

func TestWriteSandboxScripts_AdditionalBindings(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-job-bind",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-build.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		AdditionalBindings: []model.BindMount{
			{Source: "/home/user/.local/bin"},
			{Source: "/home/user/.local/share/go"},
			{Source: "/home/user/go"},
		},
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-job-bind"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	// Verify setup script mounts additional bindings
	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)

	for _, binding := range cfg.AdditionalBindings {
		if !strings.Contains(setup, fmt.Sprintf("mount --bind %s \"$ROOT%s\"", binding.Source, binding.Source)) {
			t.Errorf("setup script missing bind mount for %s", binding.Source)
		}
		if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s\"", binding.Source)) {
			t.Errorf("setup script missing read-only remount for %s", binding.Source)
		}
	}

	// Verify inner script PATH includes additional binding paths
	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	inner := string(innerContent)

	// /home/user/.local/bin ends with /bin → added directly
	if !strings.Contains(inner, "/home/user/.local/bin") {
		t.Error("inner script PATH missing /home/user/.local/bin")
	}
	// /home/user/.local/share/go → /home/user/.local/share/go/bin
	if !strings.Contains(inner, "/home/user/.local/share/go/bin") {
		t.Error("inner script PATH missing /home/user/.local/share/go/bin")
	}
	// /home/user/go → /home/user/go/bin
	if !strings.Contains(inner, "/home/user/go/bin") {
		t.Error("inner script PATH missing /home/user/go/bin")
	}
}

// --- Append the following test functions to wrapper_test.go ---
// Also modify TestWriteSandboxScripts config to include TaskID: "task-abc-123"
// and add assertions for BOID_TASK_ID and BOID_JOB_ID in the inner script.

func TestWriteSandboxScripts_Command(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-cmd-001",
		TaskID:       "task-cmd-001",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HomeDir:      "/home/user",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		Command:      "go test ./...",
		Env: map[string]string{
			"GOPATH": "/home/user/go",
		},
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-cmd-001"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	// Inner script: should run the command and have exit trap
	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	inner := string(innerContent)

	if !strings.Contains(inner, "go test ./...") {
		t.Error("inner script missing command 'go test ./...'")
	}
	if strings.Contains(inner, "boid job done") {
		t.Error("inner script must not have job done trap in command mode")
	}
	if strings.Contains(inner, "exec /bin/bash") {
		t.Error("inner script should not have exec /bin/bash in command mode")
	}
	if strings.Contains(inner, ".boid/hooks/") {
		t.Error("inner script should not invoke hook in command mode")
	}
	if !strings.Contains(inner, "BOID_TASK_ID=task-cmd-001") {
		t.Error("inner script missing BOID_TASK_ID")
	}
	if !strings.Contains(inner, "BOID_JOB_ID=test-cmd-001") {
		t.Error("inner script missing BOID_JOB_ID")
	}
	if !strings.Contains(inner, "HOME=/home/user") {
		t.Error("inner script missing HOME=/home/user")
	}

	// Setup script
	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)

	// .boid directory should be mounted read-only
	if !strings.Contains(setup, fmt.Sprintf("mount --bind %s/.boid \"$ROOT%s/.boid\"", cfg.ProjectDir, cfg.ProjectDir)) {
		t.Error("setup script missing .boid bind mount in command mode")
	}
	if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s/.boid\"", cfg.ProjectDir)) {
		t.Error("setup script missing .boid read-only remount in command mode")
	}
	// Should not mount hooks directory in command mode
	if strings.Contains(setup, ".boid/hooks") {
		t.Error("setup script must not mount hooks directory in command mode")
	}
	// Should have HOME tmpfs
	if !strings.Contains(setup, "mount -t tmpfs tmpfs \"$ROOT/home/user\"") {
		t.Error("setup script missing HOME tmpfs mount")
	}
	// Should use unshare --user instead of chroot
	if !strings.Contains(setup, "unshare --user --map-user=1000 --map-group=1000") {
		t.Error("setup script missing unshare --user")
	}
}

func TestWriteSandboxScripts_HookRole(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-hook-role",
		TaskID:       "task-hook-1",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		BrokerSocket: "/run/boid/broker.sock",
		BrokerToken:  "test-token-hook",
		Role:         "hook",
		PayloadJSON:  `{"prompt":"do stuff"}`,
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-hook-role"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	inner := string(innerContent)

	// Should have BOID_BROKER_TOKEN
	if !strings.Contains(inner, "BOID_BROKER_TOKEN=test-token-hook") {
		t.Error("inner script missing BOID_BROKER_TOKEN")
	}

	// Should NOT have BOID_TASK_ID, BOID_JOB_ID, BOID_SOCKET
	if strings.Contains(inner, "BOID_TASK_ID=") {
		t.Error("hook role inner script should NOT contain BOID_TASK_ID")
	}
	if strings.Contains(inner, "BOID_JOB_ID=") {
		t.Error("hook role inner script should NOT contain BOID_JOB_ID")
	}
	if strings.Contains(inner, "BOID_SOCKET=") {
		t.Error("hook role inner script should NOT contain BOID_SOCKET")
	}

	// Should capture stdout and use token-based job done
	if !strings.Contains(inner, "/tmp/boid-output") {
		t.Error("hook role inner script should capture stdout to /tmp/boid-output")
	}
	if !strings.Contains(inner, "boid job done test-hook-role --exit-code") {
		t.Error("hook role inner script should have boid job done with job ID")
	}

	// Should pipe payload to stdin
	if !strings.Contains(inner, `prompt`) {
		t.Error("hook role inner script should contain payload for stdin piping")
	}

	// Setup script: should NOT mount server socket
	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)
	if strings.Contains(setup, "server.sock") {
		t.Error("hook role should NOT mount server socket")
	}
	// Should still mount broker socket
	if !strings.Contains(setup, "broker.sock") {
		t.Error("hook role should mount broker socket")
	}
}

func TestWriteSandboxScripts_GateRole(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-gate-role",
		TaskID:       "task-gate-1",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		BrokerSocket: "/run/boid/broker.sock",
		BrokerToken:  "test-token-gate",
		Role:         "gate",
		TaskJSON:     `{"id":"task-gate-1","status":"executing","payload":{}}`,
		HostCommands: []string{"git", "gh"},
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-gate-role"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	inner := string(innerContent)

	// Should have BOID_BROKER_TOKEN only
	if !strings.Contains(inner, "BOID_BROKER_TOKEN=test-token-gate") {
		t.Error("gate inner script missing BOID_BROKER_TOKEN")
	}
	if strings.Contains(inner, "BOID_TASK_ID=") {
		t.Error("gate inner script should NOT contain BOID_TASK_ID")
	}
	if strings.Contains(inner, "BOID_SOCKET=") {
		t.Error("gate inner script should NOT contain BOID_SOCKET")
	}

	// Should pipe task JSON to stdin
	if !strings.Contains(inner, `task-gate-1`) {
		t.Error("gate inner script should contain task JSON for stdin piping")
	}

	// Setup script: no server socket, no project dir
	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)
	if strings.Contains(setup, "server.sock") {
		t.Error("gate should NOT mount server socket")
	}
	// Gate should NOT mount project dir
	if strings.Contains(setup, fmt.Sprintf("mount --bind %s \"$ROOT%s\"", cfg.ProjectDir, cfg.ProjectDir)) {
		t.Error("gate should NOT mount project dir")
	}
}

func TestWriteSandboxScripts_ReadonlyHook(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-ro-hook",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "review.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		BrokerSocket: "/run/boid/broker.sock",
		BrokerToken:  "test-token-ro",
		Role:         "hook",
		Readonly:     true,
		PayloadJSON:  `{}`,
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-ro-hook"
	setupPath := prefix + "-setup.sh"
	innerPath := prefix + "-inner.sh"
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)

	// Working dir should be mounted read-only
	if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s\"", cfg.ProjectDir)) {
		t.Error("readonly hook should mount project dir as read-only")
	}
}

// TestWriteSandboxScripts_TaskIDAndJobID verifies BOID_TASK_ID and BOID_JOB_ID are exported.
func TestWriteSandboxScripts_TaskIDAndJobID(t *testing.T) {
	cfg := job.WrapperConfig{
		JobID:        "test-job-ids",
		TaskID:       "task-abc-123",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
	}

	outerPath, err := job.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-job-ids"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		os.Remove(outerPath)
		os.Remove(setupPath)
		os.Remove(innerPath)
	})

	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	inner := string(innerContent)

	if !strings.Contains(inner, "BOID_TASK_ID=task-abc-123") {
		t.Error("inner script missing BOID_TASK_ID=task-abc-123")
	}
	if !strings.Contains(inner, "BOID_JOB_ID=test-job-ids") {
		t.Error("inner script missing BOID_JOB_ID=test-job-ids")
	}
}
