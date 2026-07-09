package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/testutil"
)

func TestResolveProjectRefIO_SingleMatch(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir := writeProjectRefTestProject(t, "unique-proj-xyz")
	var proj struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, &proj); err != nil {
		t.Fatalf("create project: %v", err)
	}

	got, err := resolveProjectRefIO(ts.Client, nil, false, nil, proj.ID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != proj.ID {
		t.Errorf("got ID %q, want %q", got.ID, proj.ID)
	}
}

func TestResolveProjectRefIO_MultiMatch_TTY(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir1 := writeProjectRefTestProject(t, "boid-alpha")
	dir2 := writeProjectRefTestProject(t, "boid-beta")

	var p1, p2 struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir1}, &p1); err != nil {
		t.Fatalf("create project 1: %v", err)
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir2}, &p2); err != nil {
		t.Fatalf("create project 2: %v", err)
	}

	// Select the second candidate
	in := bytes.NewReader([]byte("2\n"))
	var out bytes.Buffer

	got, err := resolveProjectRefIO(ts.Client, in, true, &out, "boid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != p1.ID && got.ID != p2.ID {
		t.Errorf("unexpected project ID %q (want one of %q or %q)", got.ID, p1.ID, p2.ID)
	}
	outStr := out.String()
	if !strings.Contains(outStr, "boid-alpha") || !strings.Contains(outStr, "boid-beta") {
		t.Errorf("expected candidates in output:\n%s", outStr)
	}
}

func TestResolveProjectRefIO_MultiMatch_NonTTY(t *testing.T) {
	ts := testutil.NewTestServer(t)

	dir1 := writeProjectRefTestProject(t, "boid-gamma")
	dir2 := writeProjectRefTestProject(t, "boid-delta")

	for _, dir := range []string{dir1, dir2} {
		if err := ts.Client.Do("POST", "/api/projects", map[string]string{"work_dir": dir}, nil); err != nil {
			t.Fatalf("create project: %v", err)
		}
	}

	got, err := resolveProjectRefIO(ts.Client, nil, false, nil, "boid-")
	if got != nil {
		t.Errorf("expected nil project, got %+v", got)
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "multiple projects match") {
		t.Errorf("error should mention multiple matches: %q", errStr)
	}
	if !strings.Contains(errStr, "boid-gamma") || !strings.Contains(errStr, "boid-delta") {
		t.Errorf("error should list candidates: %q", errStr)
	}
}

func TestResolveProjectRefIO_NotFound(t *testing.T) {
	ts := testutil.NewTestServer(t)

	got, err := resolveProjectRefIO(ts.Client, nil, false, nil, "nonexistent-project-xyz-99999")
	if got != nil {
		t.Errorf("expected nil project, got %+v", got)
	}
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

// writeProjectRefTestProject creates a temp dir with .boid/project.yaml for testing.
func writeProjectRefTestProject(t *testing.T, name string) string {
	t.Helper()
	dir := t.TempDir()
	testutil.InitGitRepoWithOrigin(t, dir)
	boidDir := filepath.Join(dir, ".boid")
	if err := os.MkdirAll(boidDir, 0o755); err != nil {
		t.Fatalf("mkdir .boid: %v", err)
	}
	projectYAML := "id: " + name + "\nname: " + name + "\n"
	if err := os.WriteFile(filepath.Join(boidDir, "project.yaml"), []byte(projectYAML), 0o644); err != nil {
		t.Fatalf("write project.yaml: %v", err)
	}
	return dir
}
