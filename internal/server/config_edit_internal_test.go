package server

import (
	"path/filepath"
	"testing"

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

// TestApplyDynamicConfigLocked_UnregisteredRestartRequiredLeaf_Panics proves
// the runtime half of the same contract directly: a ReloadRestartRequired
// schema leaf registered in neither map makes applyDynamicConfigLocked
// panic (rather than silently produce no warning) the moment that leaf's
// value actually changes between oldCfg and newCfg. Uses a throwaway schema
// entry spliced into config.Schema for the duration of the test (restored
// via t.Cleanup) rather than mutating either production map, so this test
// can never accidentally leave a permanent gap in real coverage behind.
func TestApplyDynamicConfigLocked_UnregisteredRestartRequiredLeaf_Panics(t *testing.T) {
	const bogusPath = "test.only.unregistered_restart_leaf"
	original := config.Schema
	config.Schema = append(append([]config.FieldSpec(nil), original...), config.FieldSpec{
		Path:   bogusPath,
		Kind:   config.KindBool,
		Reload: config.ReloadRestartRequired,
	})
	t.Cleanup(func() { config.Schema = original })

	srv := newInternalConfigTestServer(t)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected applyDynamicConfigLocked to panic on an unregistered ReloadRestartRequired schema leaf")
		}
	}()
	// oldCfg=nil (defensive first-ever-call path, per applyDynamicConfigLocked's
	// own doc comment) vs a freshly-parsed newCfg: extract(oldCfgSafe) would
	// need a restartFieldExtractors entry to even be called, which bogusPath
	// deliberately has none of — so this must panic before ever comparing
	// values, regardless of what oldCfg/newCfg actually contain.
	newCfg, err := config.ValidateYAML(nil)
	if err != nil {
		t.Fatalf("ValidateYAML(nil): %v", err)
	}
	srv.applyDynamicConfigLocked(nil, newCfg)
}
