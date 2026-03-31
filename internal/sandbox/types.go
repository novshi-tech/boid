package sandbox

// BindMount describes a host path to bind-mount into the sandbox.
type BindMount struct {
	Source string
	Mode   string
}
