package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/novshi-tech/boid/internal/dispatcher"
)

// This file is this package's stand-in for the two things a real sandbox
// backend does for a workspace home (PR5/PR6 of
// docs/plans/workspace-home-volume-persistence.md 論点 b/c): create the named
// volume that holds it, and run the preparation container against that volume.
//
// Every fake backend.SandboxBackend wired into a Server via Config.Backend
// needs both, because dispatcher.Runner discovers the capability by
// type-asserting dispatcher.WorkspaceInitExecutor on its backend and fails the
// dispatch outright when it is missing — deliberately, since silently skipping
// the preparation would hand the agent an unprepared $HOME.
//
// The stand-in models a docker volume as a directory, and its identity label as
// a sibling file. Modelling the identity at all (rather than returning a
// constant) is what keeps these fakes honest about the property PR6 rests on:
// VolumeCreate is idempotent and reports the EXISTING volume's label, so a
// second dispatch sees the same identity and does NOT re-run init.
//
// The wrapper's own semantics are covered in internal/dispatcher
// (workspace_init_test.go, against a real bash) and what boid asks the engine
// for is covered in container_backend_workspace_init_test.go; nothing here is
// asserting on either.

// workspaceHomeVolumeStubRoot is where the modelled volumes live. It hangs off
// XDG_DATA_HOME so it lands inside whatever throwaway home testutil/homeenv
// (or an individual test's t.Setenv) established, and is removed with it —
// there is nothing process-global to clean up, and two tests never share a
// "volume".
func workspaceHomeVolumeStubRoot() string {
	root := os.Getenv("XDG_DATA_HOME")
	if root == "" {
		root = os.TempDir()
	}
	return filepath.Join(root, "boid-test-volumes")
}

func workspaceHomeVolumeStubDir(name string) string {
	return filepath.Join(workspaceHomeVolumeStubRoot(), name)
}

// ensureWorkspaceHomeVolumeStub models VolumeCreate: idempotent, and on an
// already-existing volume it reports the identity that volume was created with
// rather than the candidate offered now.
func ensureWorkspaceHomeVolumeStub(req dispatcher.WorkspaceHomeVolumeRequest) (string, error) {
	dir := workspaceHomeVolumeStubDir(req.Name)
	idPath := dir + ".id"
	if data, err := os.ReadFile(idPath); err == nil {
		return strings.TrimSpace(string(data)), nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(idPath, []byte(req.CandidateID), 0o600); err != nil {
		return "", err
	}
	return req.CandidateID, nil
}

// runWorkspaceInitStub runs the wrapper the dispatcher assembled under a real
// bash rather than returning nil, so the home ends up in the state production
// leaves it in (skeleton present) and a second dispatch does not re-run init.
// There is no mount here, so HOME/BOID_WORKSPACE_HOME are rewritten from the
// request's container-side target to the directory standing in for its volume.
func runWorkspaceInitStub(ctx context.Context, req dispatcher.WorkspaceInitRequest) error {
	homeDir := workspaceHomeVolumeStubDir(req.HomeSource)
	if err := os.MkdirAll(homeDir, 0o700); err != nil {
		return err
	}
	env := make([]string, 0, len(req.Env)+1)
	for k, v := range req.Env {
		if v == req.HomeTarget {
			v = homeDir
		}
		env = append(env, k+"="+v)
	}
	env = append(env, "PATH="+os.Getenv("PATH"))

	cmd := exec.CommandContext(ctx, "/bin/bash", "-s")
	cmd.Stdin = strings.NewReader(req.Script)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("workspace init (test stand-in) failed: %w\n%s", err, out)
	}
	return nil
}
