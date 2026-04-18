package sandbox_test

import (
	"os"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
)

// read returns the contents of a generated script path. test-scoped cleanup
// removes the 3 scripts produced by WriteSandboxScripts.
func readScripts(t *testing.T, outerPath string) (outer, setup, inner string) {
	t.Helper()
	prefix := strings.TrimSuffix(outerPath, "-outer.sh")
	innerPath := prefix + "-inner.sh"
	setupPath := prefix + "-setup.sh"
	t.Cleanup(func() {
		_ = os.Remove(outerPath)
		_ = os.Remove(setupPath)
		_ = os.Remove(innerPath)
	})

	outerBytes, err := os.ReadFile(outerPath)
	if err != nil {
		t.Fatalf("read outer: %v", err)
	}
	setupBytes, err := os.ReadFile(setupPath)
	if err != nil {
		t.Fatalf("read setup: %v", err)
	}
	innerBytes, err := os.ReadFile(innerPath)
	if err != nil {
		t.Fatalf("read inner: %v", err)
	}
	return string(outerBytes), string(setupBytes), string(innerBytes)
}

func TestWriteSandboxScripts_Basic(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-basic-001",
		TaskID:          "task-1",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		Argv:            []string{"/home/user/proj/.boid/hooks/run.sh"},
	}

	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, setup, inner := readScripts(t, outerPath)

	if !strings.Contains(inner, "export HOME=/home/user\n") {
		t.Errorf("inner: HOME export missing\n%s", inner)
	}
	if !strings.Contains(inner, "export BOID_TASK_ID=task-1\n") {
		t.Errorf("inner: BOID_TASK_ID missing\n%s", inner)
	}
	if !strings.Contains(inner, "\ncd /home/user/proj\n") {
		t.Errorf("inner: cd missing\n%s", inner)
	}
	if !strings.Contains(setup, "mount --bind /home/user/proj") {
		t.Errorf("setup: project bind-mount missing\n%s", setup)
	}
}

func TestWriteSandboxScripts_TTY(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-tty-001",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		TTY:             true,
		Argv:            []string{"/bin/bash"},
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	outer, _, _ := readScripts(t, outerPath)

	// TTY=true uses the stderr-preserving pasta invocation.
	if !strings.Contains(outer, "exec 3>&2") {
		t.Errorf("outer should save stderr for TTY mode\n%s", outer)
	}
}

func TestWriteSandboxScripts_Proxy(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-proxy-001",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		ProxyPort:       8888,
		Argv:            []string{"/bin/true"},
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, setup, inner := readScripts(t, outerPath)

	if !strings.Contains(setup, "nft add table inet filter") {
		t.Errorf("setup: missing nft rules\n%s", setup)
	}
	if !strings.Contains(inner, "export http_proxy=http://10.0.2.2:8888") {
		t.Errorf("inner: http_proxy missing\n%s", inner)
	}
}

func TestWriteSandboxScripts_NoProxy(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-no-proxy-001",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		Argv:            []string{"/bin/true"},
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, setup, inner := readScripts(t, outerPath)

	if strings.Contains(setup, "nft add") {
		t.Errorf("setup: nft rules should be absent without proxy\n%s", setup)
	}
	if strings.Contains(inner, "http_proxy=") {
		t.Errorf("inner: http_proxy should be absent without proxy\n%s", inner)
	}
}

func TestWriteSandboxScripts_WorkspaceDirs(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-ws-001",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		WorkspaceDirs:   map[string]string{"peer": "/home/user/peer"},
		Argv:            []string{"/bin/true"},
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, setup, _ := readScripts(t, outerPath)

	if !strings.Contains(setup, "mount --bind /home/user/peer") {
		t.Errorf("setup: peer bind-mount missing\n%s", setup)
	}
	if !strings.Contains(setup, "mount -o remount,bind,ro \"$ROOT/home/user/peer\"") {
		t.Errorf("setup: peer should be remounted read-only\n%s", setup)
	}
}

func TestWriteSandboxScripts_AdditionalBindings(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-add-001",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		AdditionalBindings: []sandbox.BindMount{
			{Source: "/opt/tools"},
			{Source: "/opt/data", Mode: "rw"},
			{Source: "/host/file", Target: "/run/boid/file", IsFile: true},
		},
		Argv: []string{"/bin/true"},
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, setup, _ := readScripts(t, outerPath)

	if !strings.Contains(setup, "mount --bind /opt/tools \"$ROOT/opt/tools\"") {
		t.Errorf("setup: /opt/tools mount missing\n%s", setup)
	}
	if !strings.Contains(setup, "mount -o remount,bind,ro \"$ROOT/opt/tools\"") {
		t.Errorf("setup: /opt/tools should be read-only\n%s", setup)
	}
	if !strings.Contains(setup, "mount --bind /opt/data \"$ROOT/opt/data\"") {
		t.Errorf("setup: /opt/data mount missing\n%s", setup)
	}
	// Target + IsFile: bind to a different path, treat as file
	if !strings.Contains(setup, "mount --bind /host/file \"$ROOT/run/boid/file\"") {
		t.Errorf("setup: Target rewrite not applied\n%s", setup)
	}
	if !strings.Contains(setup, "touch \"$ROOT/run/boid/file\"") {
		t.Errorf("setup: IsFile should touch target before bind\n%s", setup)
	}
}

func TestWriteSandboxScripts_ArgvExec(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-argv-exec",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		Argv:            []string{"go", "test", "./..."},
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, _, inner := readScripts(t, outerPath)

	// No ExitScript → exec is used to replace the shell.
	if !strings.Contains(inner, "\nexec go test ./...\n") {
		t.Errorf("inner: expected exec argv form, got:\n%s", inner)
	}
	if strings.Contains(inner, "trap") {
		t.Errorf("inner: unexpected trap in exec mode\n%s", inner)
	}
}

func TestWriteSandboxScripts_ArgvWithExitScript(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-argv-trap",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		Argv:            []string{"/home/user/proj/.boid/hooks/run.sh"},
		ExitScript:      "boid job done my-job --exit-code $?",
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, _, inner := readScripts(t, outerPath)

	// ExitScript set → trap wraps it and entry runs without `exec` so the
	// trap fires when the entry exits.
	if !strings.Contains(inner, "trap 'boid job done my-job --exit-code $?' EXIT") {
		t.Errorf("inner: expected trap with ExitScript, got:\n%s", inner)
	}
	if strings.Contains(inner, "\nexec /home/user/proj/.boid/hooks/run.sh\n") {
		t.Errorf("inner: should not use `exec` when ExitScript is set\n%s", inner)
	}
}

func TestWriteSandboxScripts_StdinBytes(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-stdin",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		Argv:            []string{"/home/user/proj/.boid/hooks/run.sh"},
		StdinBytes:      []byte(`{"payload":"hello"}`),
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, _, inner := readScripts(t, outerPath)

	if !strings.Contains(inner, "printf '%s' '{\"payload\":\"hello\"}' | /home/user/proj/.boid/hooks/run.sh") {
		t.Errorf("inner: expected stdin pipe form, got:\n%s", inner)
	}
}

func TestWriteSandboxScripts_StdinAndStdoutCapture(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-stdin-stdout",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/tmp",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: false,
		Argv:            []string{"/opt/boid/gates/x.sh"},
		StdinBytes:      []byte(`{"task":"t"}`),
		StdoutCaptureFile: "/tmp/boid-output",
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, _, inner := readScripts(t, outerPath)

	if !strings.Contains(inner, "printf '%s' '{\"task\":\"t\"}' | /opt/boid/gates/x.sh > /tmp/boid-output") {
		t.Errorf("inner: expected stdin pipe + stdout capture, got:\n%s", inner)
	}
}

func TestWriteSandboxScripts_ContextFiles(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:            "test-context",
		ProjectID:        "proj-1",
		ProjectDir:       "/home/user/proj",
		HomeDir:          "/home/user",
		BoidBinary:       "/usr/local/bin/boid",
		MountProjectDir:  true,
		Argv:             []string{"/bin/true"},
		TaskYAML:         "id: task-1\n",
		EnvironmentYAML:  "readonly: false\n",
		InstructionsJSON: `[{"role":"main"}]`,
		PayloadJSON:      `{"hello":"world"}`,
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, _, inner := readScripts(t, outerPath)

	if !strings.Contains(inner, "/home/user/.boid/context/task.yaml") {
		t.Errorf("inner: task.yaml write missing\n%s", inner)
	}
	if !strings.Contains(inner, "/home/user/.boid/context/environment.yaml") {
		t.Errorf("inner: environment.yaml write missing\n%s", inner)
	}
	if !strings.Contains(inner, "/home/user/.boid/context/instructions.yaml") {
		t.Errorf("inner: instructions.yaml write missing\n%s", inner)
	}
	if !strings.Contains(inner, "/home/user/.boid/context/payload.yaml") {
		t.Errorf("inner: payload.yaml write missing\n%s", inner)
	}
	// payload.json is always written alongside payload.yaml now (no Interactive gate).
	if !strings.Contains(inner, "/home/user/.boid/context/payload.json") {
		t.Errorf("inner: payload.json write missing\n%s", inner)
	}
	if !strings.Contains(inner, "export BOID_INSTRUCTIONS=") {
		t.Errorf("inner: BOID_INSTRUCTIONS env missing\n%s", inner)
	}
}

func TestWriteSandboxScripts_ContextFiles_Noop(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-context-noop",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		Argv:            []string{"/bin/true"},
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, _, inner := readScripts(t, outerPath)

	if strings.Contains(inner, "/.boid/context/") {
		t.Errorf("inner: context directory should not be created when no context fields set\n%s", inner)
	}
}

func TestWriteSandboxScripts_ModelEnv(t *testing.T) {
	base := sandbox.WrapperConfig{
		JobID:           "test-model",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		Argv:            []string{"/bin/true"},
	}

	// With Model set
	cfgSet := base
	cfgSet.Model = "opus"
	outerSet, err := sandbox.WriteSandboxScripts(cfgSet)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, _, innerSet := readScripts(t, outerSet)
	if !strings.Contains(innerSet, "export BOID_MODEL=opus") {
		t.Errorf("BOID_MODEL should be exported when Model is set\n%s", innerSet)
	}

	// Without Model set
	cfgUnset := base
	cfgUnset.JobID = "test-model-unset"
	outerUnset, err := sandbox.WriteSandboxScripts(cfgUnset)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, _, innerUnset := readScripts(t, outerUnset)
	if strings.Contains(innerUnset, "BOID_MODEL=") {
		t.Errorf("BOID_MODEL should not appear when Model is unset\n%s", innerUnset)
	}
}

func TestWriteSandboxScripts_InvokedEnvVars(t *testing.T) {
	base := sandbox.WrapperConfig{
		JobID:           "test-invoked",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		Argv:            []string{"/bin/true"},
	}

	cfgSet := base
	cfgSet.InvokedRole = "executor"
	cfgSet.InvokedName = "security"
	cfgSet.InvokedType = "execution"
	outerSet, err := sandbox.WriteSandboxScripts(cfgSet)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, _, innerSet := readScripts(t, outerSet)
	if !strings.Contains(innerSet, "export BOID_INVOKED_ROLE=executor") {
		t.Errorf("BOID_INVOKED_ROLE missing\n%s", innerSet)
	}
	if !strings.Contains(innerSet, "export BOID_INVOKED_NAME=security") {
		t.Errorf("BOID_INVOKED_NAME missing\n%s", innerSet)
	}
	if !strings.Contains(innerSet, "export BOID_INVOKED_TYPE=execution") {
		t.Errorf("BOID_INVOKED_TYPE missing\n%s", innerSet)
	}

	cfgUnset := base
	cfgUnset.JobID = "test-invoked-unset"
	outerUnset, err := sandbox.WriteSandboxScripts(cfgUnset)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, _, innerUnset := readScripts(t, outerUnset)
	if strings.Contains(innerUnset, "BOID_INVOKED_") {
		t.Errorf("BOID_INVOKED_* should not appear when all are unset\n%s", innerUnset)
	}
}

func TestWriteSandboxScripts_CustomEnv(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-env",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		Env: map[string]string{
			"MY_VAR":             "hello",
			"BOID_BROKER_SOCKET": "/run/boid/broker.sock",
		},
		Argv: []string{"/bin/true"},
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, _, inner := readScripts(t, outerPath)

	if !strings.Contains(inner, `export MY_VAR="hello"`) {
		t.Errorf("inner: MY_VAR missing\n%s", inner)
	}
	if !strings.Contains(inner, `export BOID_BROKER_SOCKET=`) {
		t.Errorf("inner: BOID_BROKER_SOCKET should be set from cfg.Env\n%s", inner)
	}
}

func TestWriteSandboxScripts_RootDirLiteralWhenSet(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-rootdir-literal",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		RootDir:         "/tmp/sandbox-custom-root",
		Argv:            []string{"/bin/true"},
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, setup, _ := readScripts(t, outerPath)

	if !strings.Contains(setup, "ROOT=/tmp/sandbox-custom-root") {
		t.Errorf("setup: ROOT should be literal when RootDir is set\n%s", setup)
	}
	if strings.Contains(setup, "mktemp -d") {
		t.Errorf("setup: mktemp should not appear when RootDir is set\n%s", setup)
	}
}

func TestWriteSandboxScripts_RootDirMktempWhenEmpty(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:           "test-rootdir-mktemp",
		ProjectID:       "proj-1",
		ProjectDir:      "/home/user/proj",
		HomeDir:         "/home/user",
		BoidBinary:      "/usr/local/bin/boid",
		MountProjectDir: true,
		Argv:            []string{"/bin/true"},
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, setup, _ := readScripts(t, outerPath)

	if !strings.Contains(setup, "ROOT=$(mktemp -d /tmp/boid-root-XXXXXX)") {
		t.Errorf("setup: mktemp fallback missing when RootDir empty\n%s", setup)
	}
}

// Gate-like invocation: no project mount, HOME=/tmp, stdin=bytes, stdout captured.
func TestWriteSandboxScripts_GateLikeInvocation(t *testing.T) {
	cfg := sandbox.WrapperConfig{
		JobID:             "test-gate-like",
		ProjectID:         "proj-1",
		ProjectDir:        "/home/user/proj",
		HomeDir:           "/tmp",
		BoidBinary:        "/usr/local/bin/boid",
		MountProjectDir:   false,
		Argv:              []string{"/opt/boid/gates/check.sh"},
		StdinBytes:        []byte(`{"task":"1"}`),
		StdoutCaptureFile: "/tmp/boid-output",
		ExitScript:        "boid job done j --exit-code $?",
		AdditionalBindings: []sandbox.BindMount{
			{Source: "/host/gates/check.sh", Target: "/opt/boid/gates/check.sh", IsFile: true},
		},
	}
	outerPath, err := sandbox.WriteSandboxScripts(cfg)
	if err != nil {
		t.Fatalf("WriteSandboxScripts: %v", err)
	}
	_, setup, inner := readScripts(t, outerPath)

	if !strings.Contains(inner, "export HOME=/tmp\n") {
		t.Errorf("gate: HOME should be /tmp\n%s", inner)
	}
	if !strings.Contains(inner, "trap 'boid job done j --exit-code $?' EXIT") {
		t.Errorf("gate: ExitScript trap missing\n%s", inner)
	}
	if !strings.Contains(inner, "| /opt/boid/gates/check.sh > /tmp/boid-output") {
		t.Errorf("gate: stdin pipe + stdout redirect missing\n%s", inner)
	}
	// project dir should NOT be bind-mounted
	if strings.Contains(setup, "mount --bind /home/user/proj \"$ROOT/home/user/proj\"") {
		t.Errorf("gate: project dir should not be bind-mounted\n%s", setup)
	}
	// workDir tmpfs should exist
	if !strings.Contains(setup, "mount -t tmpfs tmpfs \"$ROOT/home/user/proj\"") {
		t.Errorf("gate: workDir tmpfs missing\n%s", setup)
	}
	// gate script bind-mount
	if !strings.Contains(setup, "mount --bind /host/gates/check.sh \"$ROOT/opt/boid/gates/check.sh\"") {
		t.Errorf("gate: script bind-mount missing\n%s", setup)
	}
}
