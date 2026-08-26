package orchestrator_test

// docs/plans/ingestion-identity.md PR-4 (B-5): triggers[] is parsed off
// project.yaml's TOP LEVEL (J-1), through the ordinary non-strict
// ReadProjectMeta path — no KnownFields decode is added for project.yaml
// itself (that stays reserved for the workspace envelope, which triggers
// deliberately does NOT extend — see spec_types.go's Trigger doc comment).

import (
	"testing"

	projectspec "github.com/novshi-tech/boid/internal/orchestrator"
)

// writeProjectYAML is defined in spec_loader_test.go (same package).

func TestReadProjectMeta_Triggers_Valid(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  triage: {}
triggers:
  - name: intake
    every: 10m
    run: python3 scripts/intake_tick.py
  - name: sweep
    every: 1h
    run: bash scripts/sweep_tick.sh
`)

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if len(meta.Triggers) != 2 {
		t.Fatalf("Triggers = %+v, want 2 entries", meta.Triggers)
	}
	if meta.Triggers[0].Name != "intake" || meta.Triggers[0].Every != "10m" || meta.Triggers[0].Run != "python3 scripts/intake_tick.py" {
		t.Errorf("Triggers[0] = %+v, unexpected", meta.Triggers[0])
	}
	if meta.Triggers[1].Name != "sweep" || meta.Triggers[1].Every != "1h" || meta.Triggers[1].Run != "bash scripts/sweep_tick.sh" {
		t.Errorf("Triggers[1] = %+v, unexpected", meta.Triggers[1])
	}
}

func TestReadProjectMeta_Triggers_Absent_NilNotError(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
`)
	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if len(meta.Triggers) != 0 {
		t.Errorf("Triggers = %+v, want empty when project.yaml has no triggers: key", meta.Triggers)
	}
}

// TestReadProjectMeta_Triggers_MalformedEvery_RejectedAtLoadTime pins that
// ValidateTriggers actually runs from the ReadProjectMeta pipeline (not just
// as a standalone unit under direct call, trigger_validate_test.go) — a
// project.yaml with an unparseable `every:` must fail `boid project add`/
// `fetch` outright rather than the trigger silently never firing.
func TestReadProjectMeta_Triggers_MalformedEvery_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
triggers:
  - name: intake
    every: "not a duration"
    run: echo hi
`)
	if _, err := projectspec.ReadProjectMeta(dir); err == nil {
		t.Fatal("ReadProjectMeta() = nil error, want a rejection for the malformed every:")
	}
}

// TestReadProjectMeta_Triggers_OnSignals_DecodesAsString pins F3 (Opus
// review 2026-08-26): `on` is a YAML 1.1 reserved boolean-literal key
// (on/off/yes/no all decode to true/false under YAML 1.1). yaml.v3 (this
// repo's parser) follows YAML 1.2's Core Schema instead, where `on`/`off`
// are plain strings, not booleans — so `on: signals` and `on: schedule`
// decode as the strings "signals"/"schedule", not booleans, through this
// package's ACTUAL parse path (ReadProjectMeta, not a synthetic
// yaml.Unmarshal call). This is a regression pin, not new behavior: it
// documents/locks in a YAML-library quirk this PR's correctness silently
// depends on.
func TestReadProjectMeta_Triggers_OnSignals_DecodesAsString(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
triggers:
  - name: sweep
    on: signals
    every: 2m
    run: python3 -m khi.app.scan
  - name: intake
    on: schedule
    every: 10m
    run: python3 scripts/intake_tick.py
`)

	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if len(meta.Triggers) != 2 {
		t.Fatalf("Triggers = %+v, want 2 entries", meta.Triggers)
	}
	if meta.Triggers[0].On != "signals" {
		t.Errorf("Triggers[0].On = %q, want \"signals\" (YAML 1.1 reserved-boolean-key regression)", meta.Triggers[0].On)
	}
	if meta.Triggers[1].On != "schedule" {
		t.Errorf("Triggers[1].On = %q, want \"schedule\"", meta.Triggers[1].On)
	}
}

func TestReadProjectMeta_Triggers_DuplicateName_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
triggers:
  - name: intake
    every: 10m
    run: echo one
  - name: intake
    every: 1h
    run: echo two
`)
	if _, err := projectspec.ReadProjectMeta(dir); err == nil {
		t.Fatal("ReadProjectMeta() = nil error, want a rejection for the duplicate trigger name")
	}
}
