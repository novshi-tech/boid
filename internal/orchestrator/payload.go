package orchestrator

import (
	"encoding/json"

	"github.com/novshi-tech/boid/internal/projectspec"
)

func ActiveTraitTypes(raw json.RawMessage) ([]projectspec.TraitType, error) {
	return projectspec.ActiveTraitTypes(raw)
}

func TraitMergeMode(trait projectspec.TraitType) projectspec.MergeMode {
	return projectspec.TraitMergeMode(trait)
}

func ValidatePayloadPatch(patch json.RawMessage, allowedTraits []projectspec.TraitType) error {
	return projectspec.ValidatePayloadPatch(patch, allowedTraits)
}

func MergePayloadPatch(base, patch json.RawMessage, handlerID string, allowedTraits []projectspec.TraitType) (json.RawMessage, error) {
	return projectspec.MergePayloadPatch(base, patch, handlerID, allowedTraits)
}

func MergePayload(base, update json.RawMessage) (json.RawMessage, error) {
	return projectspec.MergePayload(base, update)
}
