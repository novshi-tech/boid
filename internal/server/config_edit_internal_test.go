package server

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/config"
)

// newInternalConfigTestServer is this file's own white-box (package server)
// equivalent of config_edit_test.go's newConfigTestServer — that one lives
// in package server_test (black-box) and so is not visible here.
func newInternalConfigTestServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	srv, err := New(Config{
		DBPath:     ":memory:",
		SocketPath: filepath.Join(t.TempDir(), "boid.sock"),
		HTTPAddr:   "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = srv.DB().Close() })
	return srv
}

// TestRestartFieldExtractors_ExhaustiveCoverage is the white-box
// (package server, not server_test) test that pins the "fail loud, not
// silent" contract applyDynamicConfigLocked's generic restart-warning loop
// now enforces at runtime (regression concern, codex review round 2): every
// config.Schema leaf classified config.ReloadRestartRequired must be
// registered in EITHER restartFieldExtractors (this package's generic
// warning loop compares it directly) OR restartFieldExtractorExemptions
// (a documented reason it is deliberately covered some other way instead —
// today, gateway.forges.*'s three leaves and gateway.hosts, both handled by
// changedForgeLeaves' per-id diff).
//
// This test exists so a future schema addition that forgets both is caught
// here, at `go test` time — not by the panic applyDynamicConfigLocked would
// otherwise only hit the first time some operator's `boid config set`
// actually changed that one new key in production.
func TestRestartFieldExtractors_ExhaustiveCoverage(t *testing.T) {
	for _, spec := range config.Schema {
		if spec.Reload != config.ReloadRestartRequired {
			continue
		}
		_, hasExtractor := restartFieldExtractors[spec.Path]
		reason, hasExemption := restartFieldExtractorExemptions[spec.Path]
		switch {
		case hasExtractor && hasExemption:
			t.Errorf("schema leaf %q is registered in BOTH restartFieldExtractors and restartFieldExtractorExemptions — pick one", spec.Path)
		case !hasExtractor && !hasExemption:
			t.Errorf("schema leaf %q is ReloadRestartRequired but registered in neither restartFieldExtractors nor restartFieldExtractorExemptions — applyDynamicConfigLocked will panic on it at runtime the first time it actually changes", spec.Path)
		case hasExemption && reason == "":
			t.Errorf("schema leaf %q has an empty restartFieldExtractorExemptions reason — document why it's exempt", spec.Path)
		}
	}
}

// TestVerifyRestartExtractorCoverage_UnregisteredLeaf_Panics is the
// BLOCKER regression test (codex review round 3) for the startup-time half
// of the coverage contract: a ReloadRestartRequired schema leaf registered
// in neither map makes verifyRestartExtractorCoverage panic — the check
// wire.go's buildRuntime now runs BEFORE srv.liveConfig/srv.configPath are
// set, i.e. before any config mutation can be accepted at all. Uses a
// throwaway schema entry spliced into config.Schema for the duration of the
// test (restored via t.Cleanup) rather than mutating either production map,
// so this test can never accidentally leave a permanent gap in real
// coverage behind.
func TestVerifyRestartExtractorCoverage_UnregisteredLeaf_Panics(t *testing.T) {
	const bogusPath = "test.only.unregistered_restart_leaf"
	original := config.Schema
	config.Schema = append(append([]config.FieldSpec(nil), original...), config.FieldSpec{
		Path:   bogusPath,
		Kind:   config.KindBool,
		Reload: config.ReloadRestartRequired,
	})
	t.Cleanup(func() { config.Schema = original })

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected verifyRestartExtractorCoverage to panic on an unregistered ReloadRestartRequired schema leaf")
		}
	}()
	verifyRestartExtractorCoverage()
}

// TestApplyDynamicConfigLocked_UnregisteredRestartRequiredLeaf_WarnsNotPanics
// pins the MAJOR fix (codex review round 3): applyDynamicConfigLocked's own
// coverage check is now a fail-SAFE (slog.Warn + skip that leaf's warning),
// not a fail-loud panic, because it runs AFTER applyConfigYAMLLocked has
// already written the new document to disk and swapped s.liveConfig — a
// panic there would report failure for a mutation that had already,
// irreversibly, taken effect. Coverage completeness is now verified BEFORE
// any mutation can reach this code at all (verifyRestartExtractorCoverage,
// called once from wire.go's buildRuntime at startup) — this function's own
// check degrades gracefully rather than panicking mid-request on the
// (should-be-impossible) chance it is ever reached anyway.
func TestApplyDynamicConfigLocked_UnregisteredRestartRequiredLeaf_WarnsNotPanics(t *testing.T) {
	// Server construction runs FIRST, against the real, fully-covered
	// config.Schema — buildRuntime's verifyRestartExtractorCoverage call
	// must NOT panic here (coverage is genuinely complete at this point).
	// The bogus leaf is spliced in only AFTER the server exists, so this
	// test isolates applyDynamicConfigLocked's own runtime behavior from
	// the startup check TestVerifyRestartExtractorCoverage_UnregisteredLeaf_Panics
	// already covers separately.
	srv := newInternalConfigTestServer(t)

	const bogusPath = "test.only.unregistered_restart_leaf"
	original := config.Schema
	config.Schema = append(append([]config.FieldSpec(nil), original...), config.FieldSpec{
		Path:   bogusPath,
		Kind:   config.KindBool,
		Reload: config.ReloadRestartRequired,
	})
	t.Cleanup(func() { config.Schema = original })

	newCfg, err := config.ValidateYAML(nil)
	if err != nil {
		t.Fatalf("ValidateYAML(nil): %v", err)
	}
	// Must NOT panic despite bogusPath having no restartFieldExtractors/
	// restartFieldExtractorExemptions entry.
	warnings := srv.applyDynamicConfigLocked(nil, newCfg)
	for _, w := range warnings {
		if strings.Contains(w, bogusPath) {
			t.Errorf("expected no restart warning for an unregistered leaf, got: %q", w)
		}
	}
}

// TestMutateConfig_Batch_FinalValidationFailure_LiveConfigUnchanged is the
// white-box regression test for the "batch partial-failure at final
// validation" regression concern flagged in codex review round 2 (PR #831):
// the pre-existing black-box test
// (TestMutateConfig_Batch_PartialFailureLeavesDocumentUnchanged,
// config_edit_test.go) only exercises an op that fails AT THE OP LEVEL — an
// unknown dotted-path key, which config.Set itself rejects before
// MutateConfig's single end-of-batch config.ValidateYAML call is ever
// reached. This test instead builds a batch where EVERY individual op
// succeeds at the config.Set level (both "gateway.forges.corp.host" and
// "gateway.forges.corp.secret_key" are known, correctly-typed leaves —
// config.Set has no cross-field awareness of "forge" still being unset), but
// the ONE final validation of the fully-mutated document still fails (a
// forge entry with no "forge" kind is not a valid document —
// config.go's resolveForgeConfig: `unrecognized forge ""`). Being a
// white-box (package server) test, it asserts srv.liveConfig ITSELF — not
// just the on-disk file config_edit_test.go's black-box test already checks
// — is left completely untouched: both the same pointer (applyConfigYAMLLocked
// never even reaches its liveConfig-swap line when config.ValidateYAML fails
// first) and byte-for-byte identical contents.
func TestMutateConfig_Batch_FinalValidationFailure_LiveConfigUnchanged(t *testing.T) {
	srv := newInternalConfigTestServer(t)

	before := srv.liveConfig

	_, err := srv.MutateConfig(api.ConfigMutateRequest{Ops: []api.ConfigMutateRequest{
		{Op: api.ConfigMutateSet, Key: "gateway.forges.corp.host", Value: []string{"git.corp.example"}},
		{Op: api.ConfigMutateSet, Key: "gateway.forges.corp.secret_key", Value: []string{"CORP_PAT"}},
		// Deliberately no "gateway.forges.corp.forge" op here — each op
		// above succeeds individually, but the combined document is still
		// invalid overall (no forge kind for the new "corp" entry).
	}})
	if err == nil {
		t.Fatal("expected the batch to fail final whole-document validation (a new forge entry with no \"forge\" kind is invalid)")
	}
	if !strings.Contains(err.Error(), "unrecognized forge") {
		t.Errorf("error = %v, want it to name the missing/unrecognized forge kind (confirms this failed at FINAL document validation, not at an individual op's config.Set)", err)
	}

	if srv.liveConfig != before {
		t.Errorf("srv.liveConfig pointer changed despite the batch failing final validation: before=%p after=%p", before, srv.liveConfig)
	}
	if !reflect.DeepEqual(srv.liveConfig, before) {
		t.Errorf("srv.liveConfig contents changed despite the batch failing final validation:\nbefore=%+v\nafter=%+v", before, srv.liveConfig)
	}
}
