package project

import "github.com/novshi-tech/boid/internal/projectspec"

// ReadMeta reads and validates .boid/project.yaml from the given directory.
func ReadMeta(dir string) (*ProjectMeta, error) {
	return projectspec.ReadProjectMeta(dir)
}

// ReadMetaWithKits reads project.yaml and resolves kit references.
func ReadMetaWithKits(dir string, resolver KitResolver) (*ProjectMeta, error) {
	return projectspec.ReadProjectMetaWithKits(dir, resolver)
}
