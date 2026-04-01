package sandbox

// BindMount is a plain shared DTO consumed by sandbox setup planning.
// It carries only mount source/mode data and does not encode provider behavior.
type BindMount struct {
	Source string
	Mode   string
}
