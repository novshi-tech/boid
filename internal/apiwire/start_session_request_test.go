package apiwire

import (
	"encoding/json"
	"testing"
)

// TestStartSessionRequest_CardFieldsNotJSONSettable pins that CardID/
// CardRequestID cannot be populated by decoding an external HTTP body
// (POST /api/sessions or the project-scoped route) — only server code that
// constructs a StartSessionRequest literal directly may set them. Without
// an explicit `json:"-"` tag, encoding/json would match these by field name
// (case-insensitively) even with no tag at all, so this guards a real
// bypass, not a hypothetical one.
func TestStartSessionRequest_CardFieldsNotJSONSettable(t *testing.T) {
	body := []byte(`{
		"project_id": "p1",
		"harness_type": "claude",
		"CardID": "attacker-supplied-card",
		"card_id": "attacker-supplied-card-2",
		"CardRequestID": "attacker-supplied-request",
		"card_request_id": "attacker-supplied-request-2"
	}`)
	var req StartSessionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.CardID != "" {
		t.Errorf("CardID = %q, want empty — an external JSON body must never be able to set it", req.CardID)
	}
	if req.CardRequestID != "" {
		t.Errorf("CardRequestID = %q, want empty — an external JSON body must never be able to set it", req.CardRequestID)
	}
	if req.ProjectID != "p1" || req.HarnessType != "claude" {
		t.Fatalf("ordinary fields didn't decode: %+v", req)
	}
}
