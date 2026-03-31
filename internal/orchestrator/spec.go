package orchestrator

import "github.com/novshi-tech/boid/internal/projectspec"

type TraitType = projectspec.TraitType
type MergeMode = projectspec.MergeMode
type Role = projectspec.Role

const (
	TraitPrompt       = projectspec.TraitPrompt
	TraitArtifact     = projectspec.TraitArtifact
	TraitVerification = projectspec.TraitVerification
	TraitTasks        = projectspec.TraitTasks

	MergeModeExclusive = projectspec.MergeModeExclusive
	MergeModeShared    = projectspec.MergeModeShared

	RoleHook = projectspec.RoleHook
	RoleGate = projectspec.RoleGate
)

type BindMount = projectspec.BindMount
type CommandDef = projectspec.CommandDef
type Hook = projectspec.Hook
type Gate = projectspec.Gate
type HookFireEvent = projectspec.HookFireEvent
type GateFireEvent = projectspec.GateFireEvent
type KitHooksInfo = projectspec.KitHooksInfo
type KitGatesInfo = projectspec.KitGatesInfo
type TaskBehavior = projectspec.TaskBehavior
type ProjectMeta = projectspec.ProjectMeta
type Project = projectspec.Project
type KitMeta = projectspec.KitMeta
