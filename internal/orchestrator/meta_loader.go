package orchestrator

import "github.com/novshi-tech/boid/internal/projectspec"

// ReadProjectMeta reads and validates .boid/project.yaml from the given directory.
func ReadProjectMeta(dir string) (*projectspec.ProjectMeta, error) {
	return projectspec.ReadProjectMeta(dir)
}

// ReadProjectMetaWithKits reads project.yaml and resolves kit references.
func ReadProjectMetaWithKits(dir string, resolver KitResolver) (*projectspec.ProjectMeta, error) {
	return projectspec.ReadProjectMetaWithKits(dir, resolver)
}
