package orchestrator_test

import (
	"encoding/json"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestMatchScripts(t *testing.T) {
	scripts := []orchestrator.Script{
		{ID: "notify", On: []orchestrator.ScriptTrigger{orchestrator.ScriptTriggerTaskDone}},
		{ID: "cleanup", On: []orchestrator.ScriptTrigger{orchestrator.ScriptTriggerTaskAborted}},
		{ID: "both", On: []orchestrator.ScriptTrigger{orchestrator.ScriptTriggerTaskDone, orchestrator.ScriptTriggerTaskAborted}},
	}

	got := orchestrator.MatchScripts(scripts, orchestrator.ScriptTriggerTaskDone, "dev")
	if len(got) != 2 {
		t.Fatalf("expected 2 matches, got %d", len(got))
	}
	if got[0].ID != "notify" || got[1].ID != "both" {
		t.Errorf("unexpected match IDs: %v", []string{got[0].ID, got[1].ID})
	}
}

func TestMatchScripts_NoTrigger(t *testing.T) {
	scripts := []orchestrator.Script{
		{ID: "manual", On: nil},
		{ID: "done-only", On: []orchestrator.ScriptTrigger{orchestrator.ScriptTriggerTaskDone}},
	}

	got := orchestrator.MatchScripts(scripts, orchestrator.ScriptTriggerTaskDone, "dev")
	if len(got) != 1 || got[0].ID != "done-only" {
		t.Errorf("expected only done-only, got %v", got)
	}
}

func TestMatchScripts_BehaviorFilter(t *testing.T) {
	scripts := []orchestrator.Script{
		{
			ID: "all-behaviors",
			On: []orchestrator.ScriptTrigger{orchestrator.ScriptTriggerTaskDone},
		},
		{
			ID: "dev-only",
			On: []orchestrator.ScriptTrigger{orchestrator.ScriptTriggerTaskDone},
			Filter: orchestrator.ScriptFilter{Behavior: "dev"},
		},
		{
			ID: "prod-only",
			On: []orchestrator.ScriptTrigger{orchestrator.ScriptTriggerTaskDone},
			Filter: orchestrator.ScriptFilter{Behavior: "prod"},
		},
	}

	got := orchestrator.MatchScripts(scripts, orchestrator.ScriptTriggerTaskDone, "dev")
	if len(got) != 2 {
		t.Fatalf("expected 2 matches for behavior=dev, got %d", len(got))
	}
	if got[0].ID != "all-behaviors" || got[1].ID != "dev-only" {
		t.Errorf("unexpected IDs: %v", []string{got[0].ID, got[1].ID})
	}

	gotProd := orchestrator.MatchScripts(scripts, orchestrator.ScriptTriggerTaskDone, "prod")
	if len(gotProd) != 2 {
		t.Fatalf("expected 2 matches for behavior=prod, got %d", len(gotProd))
	}
	if gotProd[0].ID != "all-behaviors" || gotProd[1].ID != "prod-only" {
		t.Errorf("unexpected IDs: %v", []string{gotProd[0].ID, gotProd[1].ID})
	}
}

func TestMatchScripts_EmptyList(t *testing.T) {
	got := orchestrator.MatchScripts(nil, orchestrator.ScriptTriggerTaskDone, "dev")
	if len(got) != 0 {
		t.Errorf("expected empty result, got %v", got)
	}
}

func TestBuildScriptTask(t *testing.T) {
	script := orchestrator.Script{
		ID:          "notify",
		Description: "Sends a notification",
	}
	parent := &orchestrator.Task{
		ID:        "parent-task-1",
		ProjectID: "proj-1",
		Behavior:  "dev",
	}

	task := orchestrator.BuildScriptTask(script, orchestrator.ScriptTriggerTaskDone, parent)

	if !task.Ephemeral {
		t.Error("Ephemeral should be true")
	}
	if task.ProjectID != parent.ProjectID {
		t.Errorf("ProjectID = %q, want %q", task.ProjectID, parent.ProjectID)
	}
	if task.Title != script.ID {
		t.Errorf("Title = %q, want %q", task.Title, script.ID)
	}
	if task.Description != script.Description {
		t.Errorf("Description = %q, want %q", task.Description, script.Description)
	}
	if task.Transition != "one-shot" {
		t.Errorf("Transition = %q, want one-shot", task.Transition)
	}
	if task.ParentID != parent.ID {
		t.Errorf("ParentID = %q, want %q", task.ParentID, parent.ID)
	}

	var payload map[string]any
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	triggerRaw, ok := payload["_trigger"]
	if !ok {
		t.Fatal("_trigger missing from payload")
	}
	trigger, ok := triggerRaw.(map[string]any)
	if !ok {
		t.Fatalf("_trigger is not a map: %T", triggerRaw)
	}
	if trigger["event"] != string(orchestrator.ScriptTriggerTaskDone) {
		t.Errorf("_trigger.event = %q, want %q", trigger["event"], orchestrator.ScriptTriggerTaskDone)
	}
	if trigger["task_id"] != parent.ID {
		t.Errorf("_trigger.task_id = %q, want %q", trigger["task_id"], parent.ID)
	}
	if trigger["project_id"] != parent.ProjectID {
		t.Errorf("_trigger.project_id = %q, want %q", trigger["project_id"], parent.ProjectID)
	}
	if trigger["behavior"] != parent.Behavior {
		t.Errorf("_trigger.behavior = %q, want %q", trigger["behavior"], parent.Behavior)
	}
}
