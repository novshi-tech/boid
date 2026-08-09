package apiwire

import (
	"encoding/json"
)

type ApplyActionRequest struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}
