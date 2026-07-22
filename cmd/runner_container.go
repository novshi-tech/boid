package cmd

import (
	"github.com/novshi-tech/boid/internal/sandbox/runner"
)

// runner-container is the Phase 6 container-backend entry point
// (docs/plans/phase6-container-backend.md §PR2): a job container's
// ENTRYPOINT execs the image-baked boid binary as
// `boid runner-container --spec ... --state ...` directly (no shim, no
// pasta relay — see runner.RunContainer's doc comment for what it does and
// deliberately does not do relative to the userns runner-outer/-inner/
// -inner-child chain in cmd/runner.go).
//
// Registered in its own init() (rather than added to cmd/runner.go's) so
// this file, and the parallel PR2/PR3/PR4 tracks that also touch the
// runner/sandbox layer, don't collide on the same source line.
//
// Still entirely inert as of PR2: nothing in the dispatcher builds a
// `boid runner-container` command line yet (that lands with the
// containerBackend in PR5, and stays config-gated off until the PR7
// cutover).
func init() {
	rootCmd.AddCommand(
		newRunnerCmd("runner-container", "Internal: container entrypoint (Phase 6, inert until PR5/PR7)", runner.RunContainer),
	)
}
