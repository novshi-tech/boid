package model

// KitHooksInfo pairs a kit's hooks directory with the hook IDs it defines.
// Used for staging hooks into a flat directory before sandbox execution.
type KitHooksInfo struct {
	HooksDir string   // absolute host-side path to kit's hooks/
	HookIDs  []string // hook IDs defined by this kit
}

// KitGatesInfo pairs a kit's gates directory with the gate IDs it defines.
type KitGatesInfo struct {
	GatesDir string   // absolute host-side path to kit's gates/
	GateIDs  []string // gate IDs defined by this kit
}
