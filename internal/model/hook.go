package model

import (
	"fmt"
	"os"
	"path/filepath"
)


// ValidHookOnValues contains the allowed values for Hook.On.
var ValidHookOnValues = map[string]bool{
	"pending":             true,
	"executing":           true,
	"verifying":           true,
	"in_review":           true,
	"collecting_feedback": true,
	"done":                true,
	"aborted":             true,
}

// ResolveHookScript finds a hook script (.sh or .py) in the given directory.
func ResolveHookScript(hooksDir, hookID string) (string, error) {
	for _, ext := range []string{".sh", ".py"} {
		p := filepath.Join(hooksDir, hookID+ext)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("script not found: %s.(sh|py)", hookID)
}

type Hook struct {
	ID             string      `yaml:"id" json:"id"`
	On             string      `yaml:"on" json:"on"`
	RequiresTraits []TraitType `yaml:"requires_traits" json:"requires_traits"`
	Requires       []string    `yaml:"requires" json:"requires"`
	ScriptPath     string      `yaml:"-" json:"-"`
}

type HookFireEvent struct {
	EventID   string
	TaskID    string
	ProjectID string
	Hook      Hook
}

type TraitType string

const (
	TraitPrompt       TraitType = "prompt"
	TraitArtifact     TraitType = "artifact"
	TraitVerification TraitType = "verification"
	TraitTasks        TraitType = "tasks"
)

// Role distinguishes hook execution context from gate execution context.
type Role string

const (
	RoleHook Role = "hook"
	RoleGate Role = "gate"
)

// Gate represents a gate script that runs after hooks complete.
// Gates have host command access but no filesystem access.
type Gate struct {
	ID             string      `yaml:"id" json:"id"`
	On             string      `yaml:"on" json:"on"`
	RequiresTraits []TraitType `yaml:"requires_traits" json:"requires_traits"`
	ScriptPath     string      `yaml:"-" json:"-"`
}

// GateFireEvent represents a gate execution event.
type GateFireEvent struct {
	EventID   string
	TaskID    string
	ProjectID string
	Gate      Gate
}

// ValidGateOnValues contains the allowed values for Gate.On.
var ValidGateOnValues = map[string]bool{
	"pending":             true,
	"executing":           true,
	"verifying":           true,
	"in_review":           true,
	"collecting_feedback": true,
	"done":                true,
	"aborted":             true,
}

// ResolveGateScript finds a gate script (.sh or .py) in the given directory.
func ResolveGateScript(gatesDir, gateID string) (string, error) {
	for _, ext := range []string{".sh", ".py"} {
		p := filepath.Join(gatesDir, gateID+ext)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("gate script not found: %s.(sh|py)", gateID)
}
