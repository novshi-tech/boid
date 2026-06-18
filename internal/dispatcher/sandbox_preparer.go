package dispatcher

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/novshi-tech/boid/internal/sandbox"
)

type sandboxPreparerImpl struct{}

// NewSandboxPreparer returns the sandbox provider adapter. It serializes the
// sandbox.Spec to a JSON file that the go-native runner (`boid runner-outer`)
// reads back; all role-aware translation lives in BuildSandboxSpec.
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

	specPath := fmt.Sprintf("/tmp/boid-%s-runner-spec.json", spec.ID)
	statePath := fmt.Sprintf("/tmp/boid-%s-runner-state.json", spec.ID)

	data, err := json.Marshal(spec)
	if err != nil {
		_ = os.RemoveAll(spec.RootDir)
		return nil, fmt.Errorf("marshal sandbox spec: %w", err)
	}
	// 0600: the spec carries the broker token and any project secrets in Env.
	if err := os.WriteFile(specPath, data, 0o600); err != nil {
		_ = os.RemoveAll(spec.RootDir)
		return nil, fmt.Errorf("write sandbox spec: %w", err)
	}

	var stagingDir string
	if len(spec.CleanupPaths) > 0 {
		stagingDir = spec.CleanupPaths[0]
	}

	return &PreparedSandbox{
		SpecPath:   specPath,
		StatePath:  statePath,
		RootDir:    spec.RootDir,
		StagingDir: stagingDir,
	}, nil
}
