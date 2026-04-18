package dispatcher

import "github.com/novshi-tech/boid/internal/sandbox"

// PreparedSandbox is the concrete launch artifact returned by a provider.
// RootDir, ScriptPaths, and StagingDir are populated so the runner can remove
// them after the sandbox runtime has exited. They are optional: zero values
// mean "nothing to clean up here" (e.g. legacy paths or provider opted out).
type PreparedSandbox struct {
	OuterPath   string
	RootDir     string
	ScriptPaths []string
	StagingDir  string
}

// SandboxPreparer prepares concrete launch artifacts from a sandbox.Spec.
// The orchestrator-owned BuildSandboxSpec builds the spec; dispatcher only
// materializes scripts and tracks artifacts.
type SandboxPreparer interface {
	PrepareSandbox(spec sandbox.Spec) (*PreparedSandbox, error)
}
