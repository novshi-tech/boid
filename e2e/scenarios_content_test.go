package e2e

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestScenariosNoStaleTaskContextFilePaths guards e2e/scenarios/ against
// re-introducing a dependency on the task-context file distribution that
// Phase 5b retired in favor of broker RPC / CLI (`boid task
// current|instructions|env|payload|attachments`). See
// docs/plans/phase5-shim-and-task-context.md "PR 分割案 > 5b" for the RPC
// migration (5b-1..5b-5) and 5b-6 (PR #803) for the file-distribution
// removal itself (`~/.boid/context/{task,instructions,environment,
// payload}.{yaml,json}` and the `~/.boid/attachments/` RO bind).
//
// Exemptions:
//
//   - Scenarios carrying a `skip` marker file are excluded wholesale.
//     `e2e/run.sh` itself never runs them by default, and their content is
//     documentation of retired/blocked behavior rather than a live
//     scenario — see e.g. e2e/scenarios/git-gateway-peer-fetch/skip, which
//     tracks the one scenario still depending on the retired
//     `$HOME/.boid/context/environment.yaml` path pending a
//     `boid workspace peers`-style RPC replacement (plan doc PR
//     breakdown item 8's open question — a known, tracked gap, not a
//     regression).
//   - Lines whose trimmed content starts with `#` (comment lines) are
//     excluded. Several scenarios legitimately mention a retired path in
//     prose while explaining the Phase 5b cutover in a hook comment (e.g.
//     git-gateway-reopen-reclone's `.boid/project.yaml`) — these aren't
//     live dependencies.
//
// Bare `payload.json` / `payload.yaml` are deliberately NOT checked as
// standalone filenames: `payload_patch.json` (the job_done file fallback —
// plan doc decisions 6/7, intentionally still supported until a later
// phase) and e2e-harness-local files unrelated to task context (e.g.
// host-command-smoke's `start-payload.json`) would false-positive on a
// naive bare-filename check. The `.boid/context/` substring pattern below
// already catches a stale `.boid/context/payload.json` reference
// specifically, which is the only shape that ever existed.
func TestScenariosNoStaleTaskContextFilePaths(t *testing.T) {
	const scenariosRoot = "scenarios"

	entries, err := os.ReadDir(scenariosRoot)
	if err != nil {
		t.Fatalf("read %s: %v", scenariosRoot, err)
	}

	patterns := []*regexp.Regexp{
		// Matches both `~/.boid/context/` and `$HOME/.boid/context/` (and
		// `${HOME}/.boid/context/`) — the retired file-distribution
		// directory, regardless of how it's spelled.
		regexp.MustCompile(`\.boid/context/`),
		regexp.MustCompile(`\benvironment\.yaml\b`),
		regexp.MustCompile(`\btask\.yaml\b`),
		regexp.MustCompile(`\binstructions\.yaml\b`),
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		scenarioDir := filepath.Join(scenariosRoot, entry.Name())
		if _, statErr := os.Stat(filepath.Join(scenarioDir, "skip")); statErr == nil {
			continue // skip-marked: see the scenario's own `skip` file for rationale
		}

		walkErr := filepath.WalkDir(scenarioDir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for i, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				for _, re := range patterns {
					if re.MatchString(line) {
						t.Errorf("%s:%d references a retired task-context file path (matched %s); use the CLI/RPC path instead (`boid task current|instructions|env|payload|attachments`):\n\t%s",
							path, i+1, re.String(), strings.TrimSpace(line))
					}
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", scenarioDir, walkErr)
		}
	}
}
