package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
)

var ValidHookOnValues = map[string]bool{
	"pending":             true,
	"executing":           true,
	"verifying":           true,
	"in_review":           true,
	"collecting_feedback": true,
	"done":                true,
	"aborted":             true,
}

var ValidGateOnValues = map[string]bool{
	"pending":             true,
	"executing":           true,
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
