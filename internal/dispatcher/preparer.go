package dispatcher

import "github.com/novshi-tech/boid/internal/sandbox"

// PreparedSandbox is the concrete launch artifact returned by a provider.
// SpecPath is the JSON sandbox spec passed to `boid runner-outer`; StatePath is
// the runner-state.json diagnostic file (retained on failure). RootDir and
// StagingDir are populated so the runner can remove them after the sandbox
// runtime has exited; zero values mean "nothing to clean up here".
type PreparedSandbox struct {
	SpecPath   string
	StatePath  string
	RootDir    string
	StagingDir string
}

// SandboxPreparer prepares concrete launch artifacts from a sandbox.Spec.
// The orchestrator-owned BuildSandboxSpec builds the spec; dispatcher only
// serializes it and tracks artifacts.
//
// Deprecated: retiring in a follow-up PR after container-backend dogfood
// stability, alongside usernsBackend (docs/plans/phase6-cutover-followups.md
// §「userns backend 撤去」) — SandboxPreparer is usernsBackend's internal spec-
// writing seam, kept in production use unchanged as of Phase 6 PR9's
// documentation-only marker.
type SandboxPreparer interface {
	PrepareSandbox(spec sandbox.Spec) (*PreparedSandbox, error)
}
