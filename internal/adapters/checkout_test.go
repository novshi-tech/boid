package adapters

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name, repo, script string
		wantError          bool
	}{
		{"repository", "github.com/o/repo", "printf '%s\\n' \"$CHECKOUT_DEST\"", false},
		{"no repository", "", "exit 99", false},
		{"checkout fails", "github.com/o/repo", "exit 1", true},
		{"empty output", "github.com/o/repo", "exit 0", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			dest := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "boid"), []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			var stderr bytes.Buffer
			rc := RunContext{Workspace: bin, Env: map[string]string{"BOID_PRIMARY_REPO": tc.repo, "CHECKOUT_DEST": dest, "BOID_BASE_BRANCH": "feature"}, Stderr: &stderr}
			got, err := PrepareWorkspace(context.Background(), rc)
			if (err != nil) != tc.wantError {
				t.Fatalf("error = %v", err)
			}
			if err != nil {
				return
			}
			want := bin
			if tc.repo != "" {
				want = dest
			}
			if got.Workspace != want {
				t.Fatalf("cwd = %q, want %q", got.Workspace, want)
			}
			if tc.repo != "" && !strings.Contains(stderr.String(), "checkout cwd="+dest) {
				t.Fatalf("missing checkout diagnostic: %s", stderr.String())
			}
		})
	}
}
