package apiwire

import (
	"encoding/json"
	"testing"
)

// TestCreateTaskRequest_CardRequestFieldsNotJSONSettable pins that
// CardRequestID/CardRequestOwnerJobID cannot be populated by decoding an
// external body — POST /api/tasks or the sandboxed `boid task create`
// CreatePatch a job supplies — only BoidOpTaskCreate's own Go code (after
// verifying ownership) may set them. Same posture as
// TestStartSessionRequest_CardFieldsNotJSONSettable for StartSessionRequest:
// without the `json:"-"` tag, encoding/json would match these by field name
// (case-insensitively, and via the snake_case form too) even with no tag at
// all, so this guards a real bypass, not a hypothetical one.
func TestCreateTaskRequest_CardRequestFieldsNotJSONSettable(t *testing.T) {
	body := []byte(`{
		"project_id": "p1",
		"title": "t",
		"CardRequestID": "attacker-supplied-request",
		"card_request_id": "attacker-supplied-request-2",
		"CardRequestOwnerJobID": "attacker-supplied-owner",
		"card_request_owner_job_id": "attacker-supplied-owner-2"
	}`)
	var req CreateTaskRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if req.CardRequestID != "" {
		t.Errorf("CardRequestID = %q, want empty — an external/sandboxed JSON body must never be able to set it", req.CardRequestID)
	}
	if req.CardRequestOwnerJobID != "" {
		t.Errorf("CardRequestOwnerJobID = %q, want empty — an external/sandboxed JSON body must never be able to set it", req.CardRequestOwnerJobID)
	}
}
