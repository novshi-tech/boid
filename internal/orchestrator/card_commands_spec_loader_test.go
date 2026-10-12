package orchestrator_test

// card_commands / card_events are parsed off project.yaml's TOP LEVEL,
// through the ordinary ReadProjectMeta path — same placement as triggers[]
// (see trigger_spec_loader_test.go). The daemon never interprets a
// card_commands key's meaning; it only validates shape at load time.

import (
	"encoding/json"
	"strings"
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

// writeProjectYAML is defined in spec_loader_test.go (same package).

// TestProjectMeta_CardEvents_NotInJSON pins that CardEvents never appears
// in ProjectMeta's JSON output, even for a project with no card_commands.
func TestProjectMeta_CardEvents_NotInJSON(t *testing.T) {
	meta := projectspec.ProjectMeta{ID: "p", Name: "P"}
	out, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(out), "card_events") {
		t.Errorf("JSON = %s, must not contain card_events", out)
	}
}
