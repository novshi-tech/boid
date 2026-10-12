package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestProjectCLI_PermissionAddUpdateAndDisplay(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())
	withProjectAddWorkspaceFlag(t, "team")
	previous := projectAddPerm
	projectAddPerm = "fetch"
	t.Cleanup(func() { projectAddPerm = previous })
	src := writeGitURLTestProject(t, "perm-project", "Permission Project")
	var out bytes.Buffer
	projectAddCmd.SetOut(&out)
	t.Cleanup(func() { projectAddCmd.SetOut(nil); projectListCmd.SetOut(nil); projectSetPermCmd.SetOut(nil) })
	if err := runProjectAdd(projectAddCmd, []string{gitFileURL(src)}); err != nil {
		t.Fatal(err)
	}
	var p orchestrator.Project
	if err := ts.Client.Do("GET", "/api/projects/perm-project", nil, &p); err != nil {
		t.Fatal(err)
	}
	if p.Perm != "fetch" {
		t.Fatalf("perm %q", p.Perm)
	}
	projectListCmd.SetOut(&out)
	if err := runProjectList(projectListCmd, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "perm=fetch") {
		t.Fatal(out.String())
	}
	detail := captureStdout(t, func() { renderProjectDetail(&p) })
	if !strings.Contains(detail, "Perm:        fetch") {
		t.Fatal(detail)
	}
	projectSetPermCmd.SetOut(&out)
	if err := runProjectSetPerm(projectSetPermCmd, []string{p.ID, "push"}); err != nil {
		t.Fatal(err)
	}
	if err := ts.Client.Do("GET", "/api/projects/perm-project", nil, &p); err != nil {
		t.Fatal(err)
	}
	if p.Perm != "push" {
		t.Fatal(p.Perm)
	}
	if err := runProjectSetPerm(projectSetPermCmd, []string{p.ID, "invalid"}); err == nil {
		t.Fatal("invalid permission accepted")
	}
}
