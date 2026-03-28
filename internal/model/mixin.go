package model

// MixinHooksInfo pairs a mixin's hooks directory with the hook IDs it defines.
// Used for staging hooks into a flat directory before sandbox execution.
type MixinHooksInfo struct {
	HooksDir string   // absolute host-side path to mixin's hooks/
	HookIDs  []string // hook IDs defined by this mixin
}
