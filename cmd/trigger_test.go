package cmd

import (
	"strings"
	"testing"
)

// TestRunTriggerRun_RequiresProjectFlag pins the CLI's own validation guard
// — no HTTP round trip should be attempted when -p/--project is missing
// (mirrors runExec's identical "-p/--project is required" guard in
// exec.go), so this needs no live daemon/client to test.
func TestRunTriggerRun_RequiresProjectFlag(t *testing.T) {
	orig := triggerRunProjectRef
	triggerRunProjectRef = ""
	defer func() { triggerRunProjectRef = orig }()

	err := runTriggerRun(triggerRunCmd, []string{"intake"})
	if err == nil {
		t.Fatal("runTriggerRun() with no -p/--project = nil error, want a rejection")
	}
	if !strings.Contains(err.Error(), "--project") {
		t.Errorf("error = %v, want it to mention --project", err)
	}
}
