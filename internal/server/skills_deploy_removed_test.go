package server_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/novshi-tech/boid/internal/server"
)

// TestNew_DoesNotDeploySkillsBesideTheDB pins the removal of a dead write
// (PR3 of docs/plans/workspace-home-volume-persistence.md, 決定 D6).
//
// server.New used to call skills.DeployAll(filepath.Dir(cfg.DBPath) +
// "/skills") on every daemon start. That directory was the SOURCE the
// claude / codex / opencode adapters' Bindings() bound into
// ~/.claude/skills/<name> — until Phase 4 PR3 (docs/plans/
// home-workspace-volume.md) retired all three implementations to nil and
// moved skill delivery to a copy-sync into the workspace HOME. The write
// survived; its only reader did not.
//
// Leaving it in place would be actively harmful now rather than merely
// wasteful: PR3 gives the embedded skills exactly one materialization point
// (<RuntimesDir>/skills, see dispatcher's embeddedSkillsDir), and a second
// tree of the same files that nothing consumes is precisely the drift this
// plan doc's 論点 e-2 had to untangle in the first place.
//
// An existing installation keeps whatever the old code already wrote there;
// nothing reads it, and nothing here deletes it.
func TestNew_DoesNotDeploySkillsBesideTheDB(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	dataDir := t.TempDir()
	srv, err := server.New(server.Config{
		DBPath:     filepath.Join(dataDir, "boid.db"),
		SocketPath: filepath.Join(t.TempDir(), "boid.sock"),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = srv.Stop() }()

	skillsDir := filepath.Join(dataDir, "skills")
	if _, err := os.Stat(skillsDir); !os.IsNotExist(err) {
		t.Errorf("server.New created %s (stat err=%v); the embedded skills have exactly one materialization point now, under the runtimes root", skillsDir, err)
	}
}
