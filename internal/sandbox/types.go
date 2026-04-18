package sandbox

// BindMount is a plain shared DTO consumed by sandbox setup planning.
// It carries only mount source/target/mode data and does not encode provider behavior.
type BindMount struct {
	Source string
	Target string // if empty, defaults to Source
	Mode   string // "rw" | "" (ro default)
	IsFile bool   // if true, treat target as a file (touch before bind, skip type detection)
}
