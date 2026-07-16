package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestRunHostCommandsList_ShowsNames(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	hostCommandsPath, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	if err := orchestrator.WriteHostCommandsConfig(hostCommandsPath, map[string]orchestrator.HostCommandSpec{
		"gh":  {Allow: []string{"pr"}},
		"aws": {Allow: []string{"s3"}},
	}); err != nil {
		t.Fatalf("WriteHostCommandsConfig: %v", err)
	}
	if err := ts.Server.ReloadHostCommands(); err != nil {
		t.Fatalf("ReloadHostCommands: %v", err)
	}

	var out bytes.Buffer
	cmd := hostCommandsListCmd
	cmd.SetOut(&out)
	if err := runHostCommandsList(cmd, nil); err != nil {
		t.Fatalf("runHostCommandsList: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "gh") {
		t.Errorf("expected 'gh' in output, got %q", got)
	}
	if !strings.Contains(got, "aws") {
		t.Errorf("expected 'aws' in output, got %q", got)
	}
}

func TestRunHostCommandsList_Empty(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	var out bytes.Buffer
	cmd := hostCommandsListCmd
	cmd.SetOut(&out)
	if err := runHostCommandsList(cmd, nil); err != nil {
		t.Fatalf("runHostCommandsList: %v", err)
	}
	if !strings.Contains(out.String(), "no host_commands") {
		t.Errorf("expected empty-state message, got %q", out.String())
	}
}

func TestRunHostCommandsReload_PicksUpHandEdit(t *testing.T) {
	ts := testutil.NewTestServer(t)
	t.Setenv("BOID_SOCKET", ts.Server.SocketPath())

	hostCommandsPath, err := orchestrator.DefaultHostCommandsPath()
	if err != nil {
		t.Fatalf("DefaultHostCommandsPath: %v", err)
	}
	if err := orchestrator.WriteHostCommandsConfig(hostCommandsPath, map[string]orchestrator.HostCommandSpec{
		"aws": {Allow: []string{"s3"}},
	}); err != nil {
		t.Fatalf("WriteHostCommandsConfig: %v", err)
	}

	var reloadOut bytes.Buffer
	reloadCmd := hostCommandsReloadCmd
	reloadCmd.SetOut(&reloadOut)
	if err := runHostCommandsReload(reloadCmd, nil); err != nil {
		t.Fatalf("runHostCommandsReload: %v", err)
	}

	var listOut bytes.Buffer
	listCmd := hostCommandsListCmd
	listCmd.SetOut(&listOut)
	if err := runHostCommandsList(listCmd, nil); err != nil {
		t.Fatalf("runHostCommandsList: %v", err)
	}
	if !strings.Contains(listOut.String(), "aws") {
		t.Errorf("expected 'aws' after reload, got %q", listOut.String())
	}
}
