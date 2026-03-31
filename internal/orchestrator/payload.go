package orchestrator

import (
	"encoding/json"

	"github.com/novshi-tech/boid/internal/model"
)

// ActiveTraitTypes re-exported from model.
func ActiveTraitTypes(raw json.RawMessage) ([]model.TraitType, error) {
	return model.ActiveTraitTypes(raw)
}

// MergePayload re-exported from model.
func MergePayload(base, update json.RawMessage) (json.RawMessage, error) {
	return model.MergePayload(base, update)
}

// MergePayloadPatch re-exported from model.
func MergePayloadPatch(base, patch json.RawMessage, hookID string, allowedTraits []model.TraitType) (json.RawMessage, error) {
	return model.MergePayloadPatch(base, patch, hookID, allowedTraits)
}
