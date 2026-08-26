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

// --- signals.sources[] → derived trigger (docs/plans/
// signal-ingest-detailed-design.md §5.1, PR-5) ---

// TestReadProjectMeta_Signals_DerivesTrigger pins the end-to-end hydrate
// path through the PUBLIC ReadProjectMeta entry point (unlike
// signal_trigger_derive_test.go's white-box unit tests on
// deriveSignalTriggers directly): a signals.sources[] entry must show up as
// an ordinary Trigger in meta.Triggers, indistinguishable to the trigger
// loop from a user-authored one except for the non-nil Connector field.
func TestReadProjectMeta_Signals_DerivesTrigger(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
signals:
  sources:
    - connector: slack/mentions
      service: slack-api
      every: 10m
      config:
        include_threads: true
`)
	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if len(meta.Triggers) != 1 {
		t.Fatalf("Triggers = %+v, want 1 derived entry", meta.Triggers)
	}
	trig := meta.Triggers[0]
	if trig.Name != "signal:slack/mentions" || trig.Every != "10m" {
		t.Errorf("derived trigger = %+v, unexpected", trig)
	}
	if trig.Connector == nil || trig.Connector.Pack != "slack" || trig.Connector.ConnectorName != "mentions" || trig.Connector.Service != "slack-api" {
		t.Errorf("Connector = %+v, unexpected", trig.Connector)
	}
}

// TestReadProjectMeta_Signals_CoexistsWithUserTriggers pins that a
// signals.sources[] derived trigger and an ordinary user-authored
// triggers[] entry both survive hydration side by side (no accidental
// overwrite of one by the other).
func TestReadProjectMeta_Signals_CoexistsWithUserTriggers(t *testing.T) {
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
signals:
  sources:
    - connector: slack/mentions
      service: slack-api
      every: 10m
`)
	meta, err := projectspec.ReadProjectMeta(dir)
	if err != nil {
		t.Fatalf("read meta: %v", err)
	}
	if len(meta.Triggers) != 2 {
		t.Fatalf("Triggers = %+v, want 2 entries (1 user + 1 derived)", meta.Triggers)
	}
	if meta.Triggers[0].Name != "sweep" || meta.Triggers[0].Connector != nil {
		t.Errorf("Triggers[0] = %+v, want the user-authored 'sweep' trigger with nil Connector", meta.Triggers[0])
	}
	if meta.Triggers[1].Name != "signal:slack/mentions" || meta.Triggers[1].Connector == nil {
		t.Errorf("Triggers[1] = %+v, want the derived 'signal:slack/mentions' trigger with non-nil Connector", meta.Triggers[1])
	}
}

// TestReadProjectMeta_Signals_CollidesWithUserTrigger_RejectedAtLoadTime pins
// that a derived trigger name colliding with a user-authored trigger name is
// caught by the SAME ValidateTriggers duplicate-name check a hand-authored
// collision hits — no separate validation pass for signals.sources.
func TestReadProjectMeta_Signals_CollidesWithUserTrigger_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
triggers:
  - name: signal:slack/mentions
    every: 5m
    run: echo not-a-connector
signals:
  sources:
    - connector: slack/mentions
      service: slack-api
      every: 10m
`)
	if _, err := projectspec.ReadProjectMeta(dir); err == nil {
		t.Fatal("ReadProjectMeta() = nil error, want a rejection for the derived/user trigger name collision")
	}
}

// TestReadProjectMeta_Signals_DuplicateSource_RejectedAtLoadTime pins the
// same collision guard for two sources naming the identical connector.
func TestReadProjectMeta_Signals_DuplicateSource_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
signals:
  sources:
    - connector: slack/mentions
      service: slack-api
      every: 10m
    - connector: slack/mentions
      service: slack-api-2
      every: 20m
`)
	if _, err := projectspec.ReadProjectMeta(dir); err == nil {
		t.Fatal("ReadProjectMeta() = nil error, want a rejection for the duplicate signals.sources entry")
	}
}

// TestReadProjectMeta_Signals_Absent_NilNotError mirrors
// TestReadProjectMeta_Triggers_Absent_NilNotError: a project.yaml with no
// `signals:` key at all must not error and must leave meta.Triggers empty.
func TestReadProjectMeta_Signals_Absent_NilNotError(t *testing.T) {
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
		t.Errorf("Triggers = %+v, want empty when project.yaml has no signals: key", meta.Triggers)
	}
}

// TestReadProjectMeta_Signals_EveryBelowFloor_RejectedAtLoadTime pins that a
// derived trigger's `every` is subject to the SAME TriggerSweepResolution
// floor a user-authored trigger's every is (ValidateTriggers runs over the
// combined list) — signals.sources gets no special exemption.
func TestReadProjectMeta_Signals_EveryBelowFloor_RejectedAtLoadTime(t *testing.T) {
	dir := t.TempDir()
	writeProjectYAML(t, dir, `
id: test-proj
name: Test Project
task_behaviors:
  dev: {}
signals:
  sources:
    - connector: slack/mentions
      service: slack-api
      every: 1s
`)
	if _, err := projectspec.ReadProjectMeta(dir); err == nil {
		t.Fatal("ReadProjectMeta() = nil error, want a rejection for every below the sweep-resolution floor")
	}
}
