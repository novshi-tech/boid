package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestResolveProjectRoot_FindsAncestor(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "repo")
	nested := filepath.Join(projectDir, "sub", "dir")
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid"), 0o755); err != nil {
		t.Fatalf("mkdir .boid: %v", err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".boid", "project.yaml"), []byte("id: proj\nname: Test\n"), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}

	got, err := resolveProjectRoot(nested)
	if err != nil {
		t.Fatalf("resolveProjectRoot: %v", err)
	}
	if got != projectDir {
		t.Fatalf("got %q, want %q", got, projectDir)
	}
}

func TestProjectLocalCommands(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(projectDir, ".boid"), 0o755); err != nil {
		t.Fatalf("mkdir .boid: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".boid", "project.yaml"), []byte("id: proj\nname: Test\n"), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}

	execRootCmd(t, "project", "local", "init", projectDir)
	execRootCmd(t, "project", "local", "add-kit", "local/dev/repro-kit", projectDir)
	execRootCmd(t, "project", "local", "set-env", "FOO", "bar", projectDir)
	execRootCmd(t, "project", "local", "add-binding", projectDir, projectDir, "--mode", "rw")

	meta, err := projectspec.ReadProjectLocalMeta(projectDir)
	if err != nil {
		t.Fatalf("ReadProjectLocalMeta: %v", err)
	}
	if len(meta.Kits.Add) != 1 || meta.Kits.Add[0] != "local/dev/repro-kit" {
		t.Fatalf("unexpected kits.add: %+v", meta.Kits.Add)
	}
	if meta.Env["FOO"] != "bar" {
		t.Fatalf("unexpected env: %+v", meta.Env)
	}
	if len(meta.AdditionalBindings) != 1 || meta.AdditionalBindings[0].Mode != "rw" {
		t.Fatalf("unexpected bindings: %+v", meta.AdditionalBindings)
	}

	execRootCmd(t, "project", "local", "show", projectDir)

	data, err := os.ReadFile(filepath.Join(projectDir, ".boid", "project.local.yaml"))
	if err != nil {
		t.Fatalf("read project.local.yaml: %v", err)
	}
	if !strings.Contains(string(data), "local/dev/repro-kit") || !strings.Contains(string(data), "FOO: bar") {
		t.Fatalf("unexpected file content: %s", data)
	}
}

func execRootCmd(t *testing.T, args ...string) string {
	t.Helper()

	oldArgs := rootCmd.Args
	oldOut := rootCmd.OutOrStdout()
	oldErr := rootCmd.ErrOrStderr()
	defer func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(oldOut)
		rootCmd.SetErr(oldErr)
		rootCmd.Args = oldArgs
		_ = projectLocalAddBindingCmd.Flags().Set("mode", "ro")
		_ = projectLocalInitCmd.Flags().Set("force", "false")
	}()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	rootCmd.SetOut(&stdout)
	rootCmd.SetErr(&stderr)
	rootCmd.SetArgs(args)

	if err := rootCmd.Execute(); err != nil {
		t.Fatalf("execute %v: %v stderr=%s", args, err, stderr.String())
	}
	return stdout.String()
}
