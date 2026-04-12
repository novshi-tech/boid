package kit

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

// DetectResult is the outcome of running a kit's detection script.
type DetectResult string

const (
	DetectNotApplicable DetectResult = ""         // script not present, failed, or printed an unknown value
	DetectOptional      DetectResult = "optional" // kit is a candidate — shown but not auto-selected
	DetectRequired      DetectResult = "required" // kit strongly matches — auto-selected
)

// detectTimeout caps how long each kit's detection script may run.
// Tests may override it via SetDetectTimeoutForTest.
var detectTimeout = 5 * time.Second

// SetDetectTimeoutForTest overrides detectTimeout and returns a restore
// function. Intended for tests only.
func SetDetectTimeoutForTest(d time.Duration) (restore func()) {
	prev := detectTimeout
	detectTimeout = d
	return func() { detectTimeout = prev }
}

// Detect reports whether the kit is applicable to projectDir by running
// the kit's detection script (POSIX sh) with projectDir as CWD. See
// DetectResult for the possible outcomes.
//
// Returns DetectNotApplicable when:
//   - kit.Detect is nil
//   - kit.Detect.Script is empty
//   - the script file does not exist
//   - the script exits non-zero
//   - the script exceeds detectTimeout
//   - the first line of stdout is not "required" or "optional"
func Detect(projectDir, kitDir string, kit orchestrator.KitMeta) DetectResult {
	if kit.Detect == nil || kit.Detect.Script == "" {
		return DetectNotApplicable
	}
	scriptPath := filepath.Join(kitDir, kit.Detect.Script)
	if _, err := os.Stat(scriptPath); err != nil {
		return DetectNotApplicable
	}
	ctx, cancel := context.WithTimeout(context.Background(), detectTimeout)
	defer cancel()

	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, "sh", scriptPath)
	cmd.Dir = projectDir
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return DetectNotApplicable
	}
	// Read the first line of stdout.
	firstLine := stdout.String()
	if idx := strings.IndexByte(firstLine, '\n'); idx >= 0 {
		firstLine = firstLine[:idx]
	}
	switch strings.TrimSpace(firstLine) {
	case string(DetectRequired):
		return DetectRequired
	case string(DetectOptional):
		return DetectOptional
	default:
		return DetectNotApplicable
	}
}
