package server_test

// This file pins the wiring gap a review round (Opus, S8) flagged: every
// internal/dispatcher unit test for Integration Pack skill symlinks
// (workspace_init_test.go, workspace_home_test.go, skills_overlay_test.go)
// sets Runner.Packs by hand. None of them would notice if wire.go's
// `runner.Packs = packs` (buildRuntime) were ever deleted — the feature
// would silently stop working while every one of those tests stayed green.
// This test goes through the real Server.New() -> buildRuntime() wiring
// (same pattern as exec_dispatch_connector_test.go's Pack tests) so that
// wire.go's own assignment is the thing under test, not a substitute for
// it.

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// homeInitCapturingBackend wraps noopBackend, additionally recording every
// WorkspaceInitRequest RunWorkspaceInit receives — the only way (short of
// exporting a test-only Server field, which production has no other reason
// to carry) to learn which workspace home volume this dispatch prepared,
// so the test can go look at what the wrapper script actually did there.
//
// mu guards requests even though this test's own single StartExec call
// only ever drives one RunWorkspaceInit synchronously — Opus review round
// 3 (N2) flagged the existing specCapturingBackend nearby as having the
// same unguarded-append shape, harmless today for the same reason, but a
// footgun for whoever adds a second concurrent dispatch to a test using
// either type. Fixed here rather than in specCapturingBackend too: that
// one is pre-existing and out of this change's scope.
type homeInitCapturingBackend struct {
	noopBackend
	mu       sync.Mutex
	requests []dispatcher.WorkspaceInitRequest
}

func (b *homeInitCapturingBackend) RunWorkspaceInit(ctx context.Context, req dispatcher.WorkspaceInitRequest) error {
	b.mu.Lock()
	b.requests = append(b.requests, req)
	b.mu.Unlock()
	return b.noopBackend.RunWorkspaceInit(ctx, req)
}

// lastRequest returns the most recently captured request, synchronized —
// the test-body read this replaces (`homeBackend.requests[len(...)-1]`)
// raced against the write above under -race even though, in practice, by
// the time StartExec's HTTP response comes back the dispatch has already
// completed.
func (b *homeInitCapturingBackend) lastRequest(t *testing.T) dispatcher.WorkspaceInitRequest {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.requests) == 0 {
		t.Fatal("no WorkspaceInitRequest reached the backend — resolveWorkspaceHome was not exercised by this dispatch")
	}
	return b.requests[len(b.requests)-1]
}

var _ backend.SandboxBackend = (*homeInitCapturingBackend)(nil)

// TestServer_New_WiresPacksIntoWorkspaceHomeSkillSymlinks drives a real
// exec dispatch (no Connector — an ordinary readonly exec is enough to
// force resolveWorkspaceHome) against a daemon started with a real
// on-disk Pack (writeTestPack's "slack-api" skill), and checks that the
// workspace home volume noopBackend.RunWorkspaceInit actually prepared has
// a working .claude/skills/slack-api symlink reading through to the Pack's
// own SKILL.md content — proving runner.Packs reached resolveWorkspaceHome
// through the full Server.New() -> buildRuntime() -> Runner.Dispatch path,
// not just through a hand-set field in a dispatcher-package unit test.
func TestServer_New_WiresPacksIntoWorkspaceHomeSkillSymlinks(t *testing.T) {
	homeBackend := &homeInitCapturingBackend{}
	ts, _ := newSmokeServerWithPacksBackend(t, homeBackend)
	projectDir := writeSmokeProject(t)

	var project struct {
		ID string `json:"id"`
	}
	if err := ts.Client.Do("POST", "/api/projects", map[string]any{
		"work_dir": projectDir,
	}, &project); err != nil {
		t.Fatalf("create project: %v", err)
	}

	var exec api.StartExecResult
	if err := ts.Client.Do("POST", "/api/projects/"+project.ID+"/exec", api.StartExecRequest{
		Argv:     []string{"true"},
		Readonly: true,
	}, &exec); err != nil {
		t.Fatalf("start exec: %v", err)
	}
	if exec.JobID == "" {
		t.Fatal("expected non-empty job id")
	}

	req := homeBackend.lastRequest(t)
	homeDir := noopBackendVolumeDir(req.HomeSource)

	linkPath := filepath.Join(homeDir, ".claude", "skills", "slack-api")
	info, err := os.Lstat(linkPath)
	if err != nil {
		t.Fatalf("stat .claude/skills/slack-api: %v (workspace home under %s)", err, homeDir)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf(".claude/skills/slack-api is not a symlink (mode %s) — runner.Packs likely did not reach resolveWorkspaceHome", info.Mode())
	}
	data, err := os.ReadFile(filepath.Join(linkPath, "SKILL.md"))
	if err != nil {
		t.Fatalf("read through .claude/skills/slack-api/SKILL.md: %v", err)
	}
	if string(data) != "slack api reference\n" {
		t.Errorf("content through symlink = %q, want the Pack's own SKILL.md content", data)
	}
}
