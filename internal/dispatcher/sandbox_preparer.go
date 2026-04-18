package dispatcher

import (
	"fmt"
	"os"
	"strings"

	"github.com/novshi-tech/boid/internal/sandbox"
)

type sandboxPreparerImpl struct{}

// NewSandboxPreparer returns the sandbox provider adapter.
// It is a thin wrapper over sandbox.Prepare: all role-aware translation
// lives in dispatcher.BuildSandboxSpec (this package).
func NewSandboxPreparer() SandboxPreparer {
	return sandboxPreparerImpl{}
}

func (sandboxPreparerImpl) PrepareSandbox(spec sandbox.Spec) (*PreparedSandbox, error) {
	if spec.RootDir == "" {
		rootDir, err := os.MkdirTemp("", "boid-root-")
		if err != nil {
			return nil, fmt.Errorf("create sandbox root: %w", err)
		}
		spec.RootDir = rootDir
	}

	outerPath, err := sandbox.Prepare(spec)
	if err != nil {
		_ = os.RemoveAll(spec.RootDir)
		return nil, err
	}

	prefix := strings.TrimSuffix(outerPath, "-outer.sh")
	scriptPaths := []string{
		outerPath,
		prefix + "-setup.sh",
		prefix + "-inner.sh",
	}

	var stagingDir string
	if len(spec.CleanupPaths) > 0 {
		stagingDir = spec.CleanupPaths[0]
	}

	return &PreparedSandbox{
		OuterPath:   outerPath,
		RootDir:     spec.RootDir,
		ScriptPaths: scriptPaths,
		StagingDir:  stagingDir,
	}, nil
}
