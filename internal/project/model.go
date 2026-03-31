package project

import "github.com/novshi-tech/boid/internal/model"

// Type aliases: project is the canonical location for these types.
// Underlying definitions live in model/ during the migration period.
// Once Steps 2-5 are complete, these will be redefined here directly.

type BindMount = model.BindMount
type CommandDef = model.CommandDef
type TraitType = model.TraitType
type MergeMode = model.MergeMode
type Role = model.Role
type TaskBehavior = model.TaskBehavior
type Hook = model.Hook
type Gate = model.Gate
type HookFireEvent = model.HookFireEvent
type GateFireEvent = model.GateFireEvent
type KitHooksInfo = model.KitHooksInfo
type KitGatesInfo = model.KitGatesInfo
type ProjectMeta = model.ProjectMeta
type Project = model.Project

const (
	TraitPrompt       = model.TraitPrompt
	TraitArtifact     = model.TraitArtifact
	TraitVerification = model.TraitVerification
	TraitTasks        = model.TraitTasks
)

const (
	MergeModeExclusive = model.MergeModeExclusive
	MergeModeShared    = model.MergeModeShared
)

const (
	RoleHook = model.RoleHook
	RoleGate = model.RoleGate
)

// ValidHookOnValues and ValidGateOnValues re-exported from model.
var ValidHookOnValues = model.ValidHookOnValues
var ValidGateOnValues = model.ValidGateOnValues

// ResolveHookScript re-exported from model.
func ResolveHookScript(hooksDir, hookID string) (string, error) {
	return model.ResolveHookScript(hooksDir, hookID)
}

// ResolveGateScript re-exported from model.
func ResolveGateScript(gatesDir, gateID string) (string, error) {
	return model.ResolveGateScript(gatesDir, gateID)
}

// TraitMergeMode re-exported from model.
func TraitMergeMode(t TraitType) MergeMode {
	return model.TraitMergeMode(t)
}
