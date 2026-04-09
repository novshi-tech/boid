package orchestrator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// MatchScripts returns scripts whose trigger and behavior filter match the given event.
func MatchScripts(scripts []Script, event ScriptTrigger, taskBehavior string) []Script {
	var matched []Script
	for _, s := range scripts {
		if !containsTrigger(s.On, event) {
			continue
		}
		if s.Filter.Behavior != "" && s.Filter.Behavior != taskBehavior {
			continue
		}
		matched = append(matched, s)
	}
	return matched
}

func containsTrigger(triggers []ScriptTrigger, event ScriptTrigger) bool {
	for _, t := range triggers {
		if t == event {
			return true
		}
	}
	return false
}

// BuildTriggeredScriptTask creates an ephemeral Task for the given script triggered by parentTask.
// The task payload includes a _trigger field with the event context.
func BuildTriggeredScriptTask(script Script, event ScriptTrigger, parentTask *Task) *Task {
	payload, _ := json.Marshal(map[string]any{
		"_trigger": map[string]string{
			"event":      string(event),
			"task_id":    parentTask.ID,
			"project_id": parentTask.ProjectID,
			"behavior":   parentTask.Behavior,
		},
	})
	behavior := fmt.Sprintf("_script:%s/%s", script.Kit, script.ID)
	return &Task{
		ProjectID:   parentTask.ProjectID,
		Title:       fmt.Sprintf("script: %s/%s", script.Kit, script.ID),
		Description: script.Description,
		Behavior:    behavior,
		Transition:  "one-shot",
		Status:      TaskStatusPending,
		Readonly:    true,
		Ephemeral:   true,
		ParentID:    parentTask.ID,
		Payload:     json.RawMessage(payload),
	}
}

var ValidScriptTriggerValues = map[string]bool{
	"task_done":    true,
	"task_aborted": true,
}

func ResolveScriptScript(scriptsDir, scriptID string) (string, error) {
	for _, ext := range []string{".sh", ".py"} {
		path := filepath.Join(scriptsDir, scriptID+ext)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("script not found: %s.(sh|py)", scriptID)
}

var ValidHookOnValues = map[string]bool{
	"pending":             true,
	"executing":           true,
	"reworking":           true,
	"verifying":           true,
	"in_review":           true,
	"collecting_feedback": true,
	"done":                true,
	"aborted":             true,
}

var ValidGateOnValues = map[string]bool{
	"pending":             true,
	"executing":           true,
	"reworking":           true,
	"verifying":           true,
	"in_review":           true,
	"collecting_feedback": true,
	"done":                true,
	"aborted":             true,
}

func ResolveHookScript(hooksDir, hookID string) (string, error) {
	for _, ext := range []string{".sh", ".py"} {
		path := filepath.Join(hooksDir, hookID+ext)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("script not found: %s.(sh|py)", hookID)
}

func ResolveGateScript(gatesDir, gateID string) (string, error) {
	for _, ext := range []string{".sh", ".py"} {
		path := filepath.Join(gatesDir, gateID+ext)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("gate script not found: %s.(sh|py)", gateID)
}

// BuildScriptTask creates an ephemeral task spec for a script execution.
func BuildScriptTask(script Script, projectID string, triggerPayload json.RawMessage) *Task {
	behavior := fmt.Sprintf("_script:%s/%s", script.Kit, script.ID)
	return &Task{
		ProjectID:  projectID,
		Title:      fmt.Sprintf("script: %s/%s", script.Kit, script.ID),
		Behavior:   behavior,
		Transition: "one-shot",
		Readonly:   true,
		Ephemeral:  true,
		AutoStart:  true,
		Payload:    triggerPayload,
	}
}
