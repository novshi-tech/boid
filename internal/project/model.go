package project

import (
	"encoding/json"

	"github.com/novshi-tech/boid/internal/projectspec"
)

type BindMount = projectspec.BindMount
type CommandDef = projectspec.CommandDef
type TraitType = projectspec.TraitType

const (
	TraitPrompt       = projectspec.TraitPrompt
	TraitArtifact     = projectspec.TraitArtifact
	TraitVerification = projectspec.TraitVerification
	TraitTasks        = projectspec.TraitTasks
)

type MergeMode = projectspec.MergeMode

const (
	MergeModeExclusive = projectspec.MergeModeExclusive
	MergeModeShared    = projectspec.MergeModeShared
)

func TraitMergeMode(t TraitType) MergeMode {
	return projectspec.TraitMergeMode(t)
}

func ActiveTraitTypes(raw json.RawMessage) ([]TraitType, error) {
	return projectspec.ActiveTraitTypes(raw)
}

func ValidatePayloadPatch(patch json.RawMessage, allowedTraits []TraitType) error {
	return projectspec.ValidatePayloadPatch(patch, allowedTraits)
}

func MergePayloadPatch(base, patch json.RawMessage, hookID string, allowedTraits []TraitType) (json.RawMessage, error) {
	return projectspec.MergePayloadPatch(base, patch, hookID, allowedTraits)
}

func MergePayload(base, update json.RawMessage) (json.RawMessage, error) {
	return projectspec.MergePayload(base, update)
}

type Role = projectspec.Role

const (
	RoleHook = projectspec.RoleHook
	RoleGate = projectspec.RoleGate
)

type Hook = projectspec.Hook
type Gate = projectspec.Gate
type HookFireEvent = projectspec.HookFireEvent
type GateFireEvent = projectspec.GateFireEvent

var ValidHookOnValues = projectspec.ValidHookOnValues
var ValidGateOnValues = projectspec.ValidGateOnValues

func ResolveHookScript(hooksDir, hookID string) (string, error) {
	return projectspec.ResolveHookScript(hooksDir, hookID)
}

func ResolveGateScript(gatesDir, gateID string) (string, error) {
	return projectspec.ResolveGateScript(gatesDir, gateID)
}

type KitHooksInfo = projectspec.KitHooksInfo
type KitGatesInfo = projectspec.KitGatesInfo
type TaskBehavior = projectspec.TaskBehavior
type ProjectMeta = projectspec.ProjectMeta
type Project = projectspec.Project
