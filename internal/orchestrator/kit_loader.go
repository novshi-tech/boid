package orchestrator

import "github.com/novshi-tech/boid/internal/projectspec"

// ReadKitMeta reads and validates kit.yaml from the given directory.
func ReadKitMeta(dir string) (*projectspec.KitMeta, error) {
	return projectspec.ReadKitMeta(dir)
}

// MergeKitMeta merges kit configurations into a base ProjectMeta.
func MergeKitMeta(base *projectspec.ProjectMeta, kits []*projectspec.KitMeta) *projectspec.ProjectMeta {
	return projectspec.MergeKitMeta(base, kits)
}
