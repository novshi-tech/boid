package kit_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/kit"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestDetect_NilDetect(t *testing.T) {
	kitDir := t.TempDir()
	projectDir := t.TempDir()
	k := orchestrator.KitMeta{} // Detect is nil
	if got := kit.Detect(projectDir, kitDir, k); got != kit.DetectNotApplicable {
		t.Fatalf("expected DetectNotApplicable when Detect is nil, got %q", got)
	}
}

func TestDetect_EmptyScript(t *testing.T) {
	kitDir := t.TempDir()
	projectDir := t.TempDir()
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Script: ""},
	}
	if got := kit.Detect(projectDir, kitDir, k); got != kit.DetectNotApplicable {
		t.Fatalf("expected DetectNotApplicable when Script is empty, got %q", got)
	}
}

func TestDetect_MissingScriptFile(t *testing.T) {
	kitDir := t.TempDir()
	projectDir := t.TempDir()
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Script: "does-not-exist.sh"},
	}
	if got := kit.Detect(projectDir, kitDir, k); got != kit.DetectNotApplicable {
		t.Fatalf("expected DetectNotApplicable for missing script, got %q", got)
	}
}

func writeScript(t *testing.T, kitDir, name, content string) string {
	t.Helper()
	path := filepath.Join(kitDir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return name
}

func TestDetect_Required(t *testing.T) {
	kitDir := t.TempDir()
	projectDir := t.TempDir()
	writeScript(t, kitDir, "detect.sh", "echo required\n")
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Script: "detect.sh"},
	}
	if got := kit.Detect(projectDir, kitDir, k); got != kit.DetectRequired {
		t.Fatalf("expected DetectRequired, got %q", got)
	}
}

func TestDetect_Optional(t *testing.T) {
	kitDir := t.TempDir()
	projectDir := t.TempDir()
	writeScript(t, kitDir, "detect.sh", "echo optional\n")
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Script: "detect.sh"},
	}
	if got := kit.Detect(projectDir, kitDir, k); got != kit.DetectOptional {
		t.Fatalf("expected DetectOptional, got %q", got)
	}
}

func TestDetect_EmptyOutput(t *testing.T) {
	kitDir := t.TempDir()
	projectDir := t.TempDir()
	writeScript(t, kitDir, "detect.sh", "exit 0\n")
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Script: "detect.sh"},
	}
	if got := kit.Detect(projectDir, kitDir, k); got != kit.DetectNotApplicable {
		t.Fatalf("expected DetectNotApplicable for empty output, got %q", got)
	}
}

func TestDetect_UnknownOutput(t *testing.T) {
	kitDir := t.TempDir()
	projectDir := t.TempDir()
	writeScript(t, kitDir, "detect.sh", "echo maybe\n")
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Script: "detect.sh"},
	}
	if got := kit.Detect(projectDir, kitDir, k); got != kit.DetectNotApplicable {
		t.Fatalf("expected DetectNotApplicable for unknown output, got %q", got)
	}
}

func TestDetect_ExitNonZero(t *testing.T) {
	kitDir := t.TempDir()
	projectDir := t.TempDir()
	writeScript(t, kitDir, "detect.sh", "echo required; exit 1\n")
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Script: "detect.sh"},
	}
	if got := kit.Detect(projectDir, kitDir, k); got != kit.DetectNotApplicable {
		t.Fatalf("expected DetectNotApplicable for non-zero exit, got %q", got)
	}
}

func TestDetect_WhitespaceTrimmed(t *testing.T) {
	kitDir := t.TempDir()
	projectDir := t.TempDir()
	writeScript(t, kitDir, "detect.sh", "printf '  required  \\n'\n")
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Script: "detect.sh"},
	}
	if got := kit.Detect(projectDir, kitDir, k); got != kit.DetectRequired {
		t.Fatalf("expected DetectRequired with trimmed whitespace, got %q", got)
	}
}

func TestDetect_MultilineFirstLineOnly(t *testing.T) {
	kitDir := t.TempDir()
	projectDir := t.TempDir()
	writeScript(t, kitDir, "detect.sh", "printf 'required\\nextra\\n'\n")
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Script: "detect.sh"},
	}
	if got := kit.Detect(projectDir, kitDir, k); got != kit.DetectRequired {
		t.Fatalf("expected DetectRequired (only first line), got %q", got)
	}
}

func TestDetect_CWDIsProjectDir(t *testing.T) {
	kitDir := t.TempDir()
	projectDir := t.TempDir()
	// Create a marker file in the project directory.
	if err := os.WriteFile(filepath.Join(projectDir, "marker"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	writeScript(t, kitDir, "detect.sh", "[ -e marker ] && echo required\n")
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Script: "detect.sh"},
	}
	if got := kit.Detect(projectDir, kitDir, k); got != kit.DetectRequired {
		t.Fatalf("expected DetectRequired (CWD is projectDir), got %q", got)
	}
}

func TestDetect_Timeout(t *testing.T) {
	restore := kit.SetDetectTimeoutForTest(200 * time.Millisecond)
	t.Cleanup(restore)

	kitDir := t.TempDir()
	projectDir := t.TempDir()
	writeScript(t, kitDir, "detect.sh", "sleep 10; echo required\n")
	k := orchestrator.KitMeta{
		Detect: &orchestrator.KitDetect{Script: "detect.sh"},
	}
	if got := kit.Detect(projectDir, kitDir, k); got != kit.DetectNotApplicable {
		t.Fatalf("expected DetectNotApplicable on timeout, got %q", got)
	}
}
