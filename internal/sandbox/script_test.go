package sandbox_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

func TestWriteSandboxScripts(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-job-001",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/projects/proj-1",
		HooksDir:        "/home/user/projects/proj-1/.boid/hooks",
		HookScript:      "run-agent.sh",
		BoidBinary:      "/usr/local/bin/boid",
		ServerSocket:    "/run/boid/server.sock",
		BrokerSocket:    "/run/boid/broker.sock",
		BrokerToken:     "test-token-abc",
		BuiltinCommands: []string{"boid"},
		Env: map[string]string{
			"MY_VAR": "hello",
		},
		HostCommands: []string{"git", "gh"},
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-job-001"
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"

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
	if !strings.Contains(setup, fmt.Sprintf("mount --bind %s/.boid \"$ROOT%s/.boid\"", cfg.ProjectDir, cfg.ProjectDir)) {
		t.Error("setup script missing .boid bind mount")
	}
	if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s/.boid\"", cfg.ProjectDir)) {
		t.Error("setup script missing .boid read-only remount")
	}
	if !strings.Contains(setup, fmt.Sprintf("mount --bind %s \"$ROOT%s/.boid/hooks\"", cfg.HooksDir, cfg.ProjectDir)) {
		t.Error("setup script missing hooks overlay bind mount")
	}
	if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s/.boid/hooks\"", cfg.ProjectDir)) {
		t.Error("setup script missing hooks overlay read-only remount")
	}
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
	if !strings.Contains(inner, "BOID_BUILTIN_SHIM=1") {
		t.Error("inner script missing BOID_BUILTIN_SHIM")
	}

	if !strings.Contains(setup, `mount --bind /run/boid/broker.sock "$ROOT/run/boid/broker.sock"`) {
		t.Error("setup script missing broker socket bind mount")
	}
}

func TestWriteSandboxScripts_TTY(t *testing.T) {
	cfg := sandbox.WrapperConfig{
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

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
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
	if strings.Contains(inner, "BOID_BUILTIN_SHIM=1") {
		t.Error("inner script must not export BOID_BUILTIN_SHIM without boid builtin")
	}

	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)
	if !strings.Contains(setup, fmt.Sprintf("mount --bind %s/.boid \"$ROOT%s/.boid\"", cfg.ProjectDir, cfg.ProjectDir)) {
		t.Error("setup script missing .boid bind mount in command mode")
	}
	if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s/.boid\"", cfg.ProjectDir)) {
		t.Error("setup script missing .boid read-only remount in command mode")
	}
	if strings.Contains(setup, ".boid/hooks") {
		t.Error("setup script must not mount hooks directory in command mode")
	}
	if !strings.Contains(setup, "unshare --user") {
		t.Error("setup script missing 'unshare --user'")
	}
}

func TestWriteSandboxScripts_Proxy(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:        "test-job-proxy",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		ProxyPort:    8888,
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
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
	cfg := sandbox.WrapperConfig{
		JobID:        "test-job-noproxy",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		ProxyPort:    0,
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
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

	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	if strings.Contains(string(setupContent), "nft ") {
		t.Error("setup script should not contain nftables when ProxyPort is 0")
	}

	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	if strings.Contains(string(innerContent), "http_proxy") {
		t.Error("inner script should not contain proxy env when ProxyPort is 0")
	}
}

func TestWriteSandboxScripts_WorkspaceDirs(t *testing.T) {
	cfg := sandbox.WrapperConfig{
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

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
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

	if !strings.Contains(setup, fmt.Sprintf("mount --bind %s \"$ROOT%s\"", cfg.ProjectDir, cfg.ProjectDir)) {
		t.Errorf("setup script missing project mount at host path %s", cfg.ProjectDir)
	}

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
	cfg := sandbox.WrapperConfig{
		JobID:        "test-job-bind",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-build.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		AdditionalBindings: []sandbox.BindMount{
			{Source: "/home/user/.local/bin"},
			{Source: "/home/user/.local/share/go"},
			{Source: "/home/user/go"},
		},
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
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

	innerContent, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner script: %v", err)
	}
	inner := string(innerContent)

	if !strings.Contains(inner, "/home/user/.local/bin") {
		t.Error("inner script PATH missing /home/user/.local/bin")
	}
	if !strings.Contains(inner, "/home/user/.local/share/go/bin") {
		t.Error("inner script PATH missing /home/user/.local/share/go/bin")
	}
	if !strings.Contains(inner, "/home/user/go/bin") {
		t.Error("inner script PATH missing /home/user/go/bin")
	}
}

func TestWriteSandboxScripts_Command(t *testing.T) {
	cfg := sandbox.WrapperConfig{
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

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
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

	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)

	if !strings.Contains(setup, fmt.Sprintf("mount --bind %s/.boid \"$ROOT%s/.boid\"", cfg.ProjectDir, cfg.ProjectDir)) {
		t.Error("setup script missing .boid bind mount in command mode")
	}
	if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s/.boid\"", cfg.ProjectDir)) {
		t.Error("setup script missing .boid read-only remount in command mode")
	}
	if strings.Contains(setup, ".boid/hooks") {
		t.Error("setup script must not mount hooks directory in command mode")
	}
	if !strings.Contains(setup, "mount -t tmpfs tmpfs \"$ROOT/home/user\"") {
		t.Error("setup script missing HOME tmpfs mount")
	}
	if !strings.Contains(setup, "unshare --user --map-user=1000 --map-group=1000") {
		t.Error("setup script missing unshare --user")
	}
}

func TestWriteSandboxScripts_HookRole(t *testing.T) {
	cfg := sandbox.WrapperConfig{
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

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
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

	if !strings.Contains(inner, "BOID_BROKER_TOKEN=test-token-hook") {
		t.Error("inner script missing BOID_BROKER_TOKEN")
	}
	if !strings.Contains(inner, "BOID_TASK_ID=task-hook-1") {
		t.Error("hook role inner script missing BOID_TASK_ID")
	}
	if strings.Contains(inner, "BOID_JOB_ID=") {
		t.Error("hook role inner script should NOT contain BOID_JOB_ID")
	}
	if strings.Contains(inner, "BOID_SOCKET=") {
		t.Error("hook role inner script should NOT contain BOID_SOCKET")
	}
	if !strings.Contains(inner, "/tmp/boid-output") {
		t.Error("hook role inner script should capture stdout to /tmp/boid-output")
	}
	if !strings.Contains(inner, "boid job done test-hook-role --exit-code") {
		t.Error("hook role inner script should have boid job done with job ID")
	}
	if !strings.Contains(inner, `prompt`) {
		t.Error("hook role inner script should contain payload for stdin piping")
	}

	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)
	if strings.Contains(setup, "server.sock") {
		t.Error("hook role should NOT mount server socket")
	}
	if !strings.Contains(setup, "broker.sock") {
		t.Error("hook role should mount broker socket")
	}
}

func TestWriteSandboxScripts_GateRole(t *testing.T) {
	cfg := sandbox.WrapperConfig{
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

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
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

	if !strings.Contains(inner, "BOID_BROKER_TOKEN=test-token-gate") {
		t.Error("gate inner script missing BOID_BROKER_TOKEN")
	}
	if strings.Contains(inner, "BOID_TASK_ID=") {
		t.Error("gate inner script should NOT contain BOID_TASK_ID")
	}
	if strings.Contains(inner, "BOID_SOCKET=") {
		t.Error("gate inner script should NOT contain BOID_SOCKET")
	}
	if !strings.Contains(inner, `task-gate-1`) {
		t.Error("gate inner script should contain task JSON for stdin piping")
	}

	setupContent, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup script: %v", err)
	}
	setup := string(setupContent)
	if strings.Contains(setup, "server.sock") {
		t.Error("gate should NOT mount server socket")
	}
	if strings.Contains(setup, fmt.Sprintf("mount --bind %s \"$ROOT%s\"", cfg.ProjectDir, cfg.ProjectDir)) {
		t.Error("gate should NOT mount project dir")
	}
}

func TestWriteSandboxScripts_ReadonlyHook(t *testing.T) {
	cfg := sandbox.WrapperConfig{
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

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
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

	if !strings.Contains(setup, fmt.Sprintf("mount -o remount,bind,ro \"$ROOT%s\"", cfg.ProjectDir)) {
		t.Error("readonly hook should mount project dir as read-only")
	}
}

func TestWriteSandboxScripts_HookRole_BoidInstructions(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:            "test-hook-instructions",
		TaskID:           "task-inst-1",
		ProjectID:        "proj-1",
		ProjectDir:       "/home/user/projects/proj-1",
		HooksDir:         "/home/user/projects/proj-1/.boid/hooks",
		HookScript:       "run-agent.sh",
		BoidBinary:       "/usr/local/bin/boid",
		ServerSocket:     "/run/boid/server.sock",
		BrokerSocket:     "/run/boid/broker.sock",
		BrokerToken:      "test-token-inst",
		Role:             "hook",
		PayloadJSON:      `{"prompt":"do stuff"}`,
		InstructionsJSON: `[{"role":"reviewer","type":"verification","consumer":"claude-code","message":"check style"}]`,
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-hook-instructions"
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

	if !strings.Contains(inner, "BOID_INSTRUCTIONS=") {
		t.Error("hook inner script missing BOID_INSTRUCTIONS export")
	}
	if !strings.Contains(inner, "reviewer") {
		t.Error("hook inner script BOID_INSTRUCTIONS missing role 'reviewer'")
	}
	if !strings.Contains(inner, "check style") {
		t.Error("hook inner script BOID_INSTRUCTIONS missing message 'check style'")
	}
}

func TestWriteSandboxScripts_HookRole_NoBoidInstructionsWhenEmpty(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:        "test-hook-noinst",
		TaskID:       "task-noinst-1",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		BrokerSocket: "/run/boid/broker.sock",
		BrokerToken:  "test-token-noinst",
		Role:         "hook",
		PayloadJSON:  `{"prompt":"do stuff"}`,
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-hook-noinst"
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

	if strings.Contains(inner, "BOID_INSTRUCTIONS") {
		t.Error("hook inner script should NOT contain BOID_INSTRUCTIONS when InstructionsJSON is empty")
	}
}

func TestWriteSandboxScripts_HookRole_BoidInstructions_SingleQuoteEscape(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:            "test-hook-sqescape",
		TaskID:           "task-sq-1",
		ProjectID:        "proj-1",
		ProjectDir:       "/home/user/projects/proj-1",
		HooksDir:         "/home/user/projects/proj-1/.boid/hooks",
		HookScript:       "run-agent.sh",
		BoidBinary:       "/usr/local/bin/boid",
		ServerSocket:     "/run/boid/server.sock",
		BrokerSocket:     "/run/boid/broker.sock",
		BrokerToken:      "test-token-sq",
		Role:             "hook",
		PayloadJSON:      `{}`,
		InstructionsJSON: `[{"role":"r","message":"it's important"}]`,
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-hook-sqescape"
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

	if !strings.Contains(inner, "BOID_INSTRUCTIONS=") {
		t.Error("hook inner script missing BOID_INSTRUCTIONS export")
	}
	// Verify the single quote is properly escaped ('"'"' form)
	if !strings.Contains(inner, `'"'"'`) {
		t.Error("hook inner script should escape single quote in BOID_INSTRUCTIONS value")
	}
}

func TestWriteSandboxScripts_HookRole_ContextFiles(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-hook-ctx",
		TaskID:          "task-ctx-1",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/projects/proj-1",
		HomeDir:         "/home/user",
		HooksDir:        "/home/user/projects/proj-1/.boid/hooks",
		HookScript:      "run-agent.sh",
		BoidBinary:      "/usr/local/bin/boid",
		ServerSocket:    "/run/boid/server.sock",
		BrokerSocket:    "/run/boid/broker.sock",
		BrokerToken:     "test-token-ctx",
		Role:            "hook",
		PayloadJSON:     `{"instructions":{"executor":{"type":"execution","consumer":"claude-code","message":"do it"}}}`,
		InstructionsJSON: `[{"role":"executor","type":"execution","consumer":"claude-code","message":"do it"}]`,
		TaskYAML:        "id: task-ctx-1\ntitle: Test Task\nstatus: executing\nbehavior: impl\n",
		EnvironmentYAML: "readonly: false\nworktree: false\nnetwork:\n  restricted: true\ntools:\n- git\n",
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-hook-ctx"
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

	// Context directory creation
	if !strings.Contains(inner, "mkdir -p") || !strings.Contains(inner, ".boid/context") {
		t.Error("inner script missing mkdir for .boid/context")
	}

	// task.yaml
	if !strings.Contains(inner, "task.yaml") {
		t.Error("inner script missing task.yaml write")
	}
	if !strings.Contains(inner, "task-ctx-1") {
		t.Error("inner script task.yaml missing task ID")
	}

	// instructions.json
	if !strings.Contains(inner, "instructions.json") {
		t.Error("inner script missing instructions.json write")
	}

	// payload.json
	if !strings.Contains(inner, "payload.json") {
		t.Error("inner script missing payload.json write")
	}

	// environment.yaml
	if !strings.Contains(inner, "environment.yaml") {
		t.Error("inner script missing environment.yaml write")
	}
	if !strings.Contains(inner, "restricted") {
		t.Error("inner script environment.yaml missing network info")
	}
}

func TestWriteSandboxScripts_HookRole_NoContextFilesWhenEmpty(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:        "test-hook-noctx",
		TaskID:       "task-noctx-1",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		BrokerSocket: "/run/boid/broker.sock",
		BrokerToken:  "test-token-noctx",
		Role:         "hook",
		PayloadJSON:  `{}`,
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-hook-noctx"
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

	// task.yaml should not be written when TaskYAML is empty
	if strings.Contains(inner, "task.yaml") {
		t.Error("inner script should not write task.yaml when TaskYAML is empty")
	}

	// environment.yaml should not be written when EnvironmentYAML is empty
	if strings.Contains(inner, "environment.yaml") {
		t.Error("inner script should not write environment.yaml when EnvironmentYAML is empty")
	}

	// payload.json SHOULD be written (PayloadJSON is `{}`)
	if !strings.Contains(inner, "payload.json") {
		t.Error("inner script should write payload.json even with empty object")
	}
}

func TestWriteSandboxScripts_HookRole_OutputDir(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:        "test-hook-output",
		TaskID:       "task-out-1",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HomeDir:      "/home/user",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		BrokerSocket: "/run/boid/broker.sock",
		BrokerToken:  "test-token-out",
		Role:         "hook",
		PayloadJSON:  `{"prompt":"do stuff"}`,
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-hook-output"
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

	// Must create output directory
	if !strings.Contains(inner, ".boid/output") {
		t.Error("inner script missing .boid/output directory creation")
	}

	// Must have conditional trap that prefers file-based output
	if !strings.Contains(inner, "payload_patch.json") {
		t.Error("inner script missing payload_patch.json reference in trap")
	}

	// Must still have fallback to /tmp/boid-output
	if !strings.Contains(inner, "/tmp/boid-output") {
		t.Error("inner script missing /tmp/boid-output fallback")
	}
}

func TestWriteSandboxScripts_GateRole_OutputDir(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:        "test-gate-output",
		TaskID:       "task-gout-1",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		BrokerSocket: "/run/boid/broker.sock",
		BrokerToken:  "test-token-gout",
		Role:         "gate",
		TaskJSON:     `{"id":"task-gout-1"}`,
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-gate-output"
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

	// Gate should also support file-based output
	if !strings.Contains(inner, "payload_patch.json") {
		t.Error("gate inner script missing payload_patch.json reference")
	}
}

func TestWriteSandboxScripts_GateRole_NoContextFiles(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:        "test-gate-noctx",
		TaskID:       "task-gate-noctx",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
		BrokerSocket: "/run/boid/broker.sock",
		BrokerToken:  "test-token-gate-noctx",
		Role:         "gate",
		TaskJSON:     `{"id":"task-gate-noctx"}`,
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}

	prefix := "/tmp/boid-test-gate-noctx"
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

	// Gate should not have context files (gate uses stdin for task data)
	if strings.Contains(inner, ".boid/context") {
		t.Error("gate inner script should not create .boid/context")
	}
}

func TestWriteSandboxScripts_TaskIDAndJobID(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:        "test-job-ids",
		TaskID:       "task-abc-123",
		ProjectID:    "proj-1",
		ProjectDir:   "/home/user/projects/proj-1",
		HooksDir:     "/home/user/projects/proj-1/.boid/hooks",
		HookScript:   "run-agent.sh",
		BoidBinary:   "/usr/local/bin/boid",
		ServerSocket: "/run/boid/server.sock",
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
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
