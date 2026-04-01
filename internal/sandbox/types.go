package sandbox

// BindMount is a plain shared DTO consumed by sandbox setup planning.
type BindMount struct {
	Source string
	Mode   string
}
