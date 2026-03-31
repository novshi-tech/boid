package orchestrator

import (
	"encoding/json"

	"github.com/novshi-tech/boid/internal/project"
)

func ActiveTraitTypes(raw json.RawMessage) ([]project.TraitType, error) {
	return project.ActiveTraitTypes(raw)
}

func MergePayload(base, update json.RawMessage) (json.RawMessage, error) {
	return project.MergePayload(base, update)
}

func MergePayloadPatch(base, patch json.RawMessage, hookID string, allowedTraits []project.TraitType) (json.RawMessage, error) {
	return project.MergePayloadPatch(base, patch, hookID, allowedTraits)
}
