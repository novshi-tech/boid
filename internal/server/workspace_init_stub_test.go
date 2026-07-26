package server

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/novshi-tech/boid/internal/dispatcher"
)

// runWorkspaceInitStub is this package's stand-in for the throwaway container
// that prepares a workspace home (PR5 of
// docs/plans/workspace-home-volume-persistence.md 論点 c). Every fake
// backend.SandboxBackend wired into a Server via Config.Backend needs one,
// because dispatcher.Runner discovers the capability by type-asserting
// dispatcher.WorkspaceInitExecutor on its backend and fails the dispatch
// outright when it is missing — deliberately, since silently skipping the
// preparation would hand the agent an unprepared $HOME.
//
// It runs the wrapper the dispatcher assembled under a real bash rather than
// returning nil, so the home ends up in the state production leaves it in
// (skeleton present, identity stamped) and a second dispatch does not re-run
// init. There is no mount here, so HOME/BOID_WORKSPACE_HOME are rewritten from
// the request's container-side target back to its host-side source.
//
// The wrapper's own semantics are covered in internal/dispatcher
// (workspace_init_test.go, against a real bash) and what boid asks the engine
// for is covered in container_backend_workspace_init_test.go; nothing here is
// asserting on either.
func runWorkspaceInitStub(ctx context.Context, req dispatcher.WorkspaceInitRequest) error {
	env := make([]string, 0, len(req.Env)+1)
	for k, v := range req.Env {
		if v == req.HomeTarget {
			v = req.HomeSource
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
