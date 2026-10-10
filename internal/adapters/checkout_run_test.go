package adapters_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/adapters"
	"github.com/novshi-tech/boid/internal/adapters/claude"
	"github.com/novshi-tech/boid/internal/adapters/codex"
	"github.com/novshi-tech/boid/internal/adapters/opencode"
	"github.com/novshi-tech/boid/internal/adapters/shell"
)

func TestAdapterCheckoutBeforeCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adapter adapters.HarnessAdapter
	}{
		{"claude session", claude.New()}, {"codex hook", codex.New()}, {"opencode hook", opencode.New()}, {"command hook and exec", shell.New()},
	} {
		for _, primary := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: " without repo", true: " with repo"}[primary], func(t *testing.T) {
				home := t.TempDir()
				dest := filepath.Join(home, "repo")
				count := filepath.Join(home, "count")
				bin := t.TempDir()
				checkoutScript := "#!/bin/sh\n[ \"$1\" = checkout ] || exit 99\nprintf x >> \"$CHECKOUT_COUNT\"\nmkdir \"$CHECKOUT_DEST\"\nprintf '%s\\n' \"$CHECKOUT_DEST\"\n"
				harnessScript := "#!/bin/sh\npwd\nprintf '%s\\n' \"$BOID_BASE_BRANCH\"\n"
				for name, script := range map[string]string{"boid": checkoutScript, "claude": harnessScript, "codex": harnessScript, "opencode": harnessScript} {
					if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
				}
				t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
				env := map[string]string{"CHECKOUT_DEST": dest, "CHECKOUT_COUNT": count, "BOID_BASE_BRANCH": "feature", "PATH": os.Getenv("PATH"), "HOME": home}
				if primary {
					env["BOID_PRIMARY_REPO"] = "github.com/owner/repo"
				}
				var stdout, stderr bytes.Buffer
				rc := adapters.RunContext{Workspace: home, Env: env, Argv: []string{"sh", "-c", "pwd; printf '%s\\n' \"$BOID_BASE_BRANCH\""}, Stdout: &stdout, Stderr: &stderr}
				if strings.Contains(tc.name, "hook") && tc.name != "command hook and exec" {
					rc.TaskID = "task"
				}
				result, err := tc.adapter.Run(context.Background(), rc)
				if err != nil || result.ExitCode != 0 {
					t.Fatalf("run: %v %+v stderr=%s", err, result, stderr.String())
				}
				want := home
				if primary {
					want = dest
				}
				if strings.TrimSpace(stdout.String()) != want+"\nfeature" {
					t.Fatalf("output=%q", stdout.String())
				}
				calls, err := os.ReadFile(count)
				if primary && (err != nil || string(calls) != "x") {
					t.Fatalf("checkout calls=%q %v", calls, err)
				}
				if !primary && !os.IsNotExist(err) {
					t.Fatalf("checkout ran without a primary repo: %v", err)
				}
			})
		}
	}
}

func TestAdapterReportsActualBranchWithoutSelectingBase(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"-c", "user.name=test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-m", "seed"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "boid"), []byte("#!/bin/sh\nprintf '%s\\n' \"$CHECKOUT_DEST\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	var stderr bytes.Buffer
	result, err := shell.New().Run(context.Background(), adapters.RunContext{
		Workspace: dir, Env: map[string]string{"PATH": os.Getenv("PATH"), "BOID_PRIMARY_REPO": "github.com/owner/repo", "BOID_BASE_BRANCH": "feature", "CHECKOUT_DEST": dir},
		Argv: []string{"git", "switch", "-c", "boid/12345678"}, Stderr: &stderr,
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("run: %v %+v", err, result)
	}
	if !strings.Contains(stderr.String(), "phase=startup cwd="+dir+" branch=main base_branch=feature") || !strings.Contains(stderr.String(), "phase=exit cwd="+dir+" branch=boid/12345678 base_branch=feature") {
		t.Fatalf("branch diagnostics=%s", stderr.String())
	}
}
