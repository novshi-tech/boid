package server

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type capturingBroker struct {
	token string
	ctx   sandbox.TokenContext
}

func (b *capturingBroker) RegisterCommands(commands map[string]orchestrator.CommandDef, builtinPolicies map[string]sandbox.BuiltinPolicy, ctx sandbox.TokenContext, resolve dispatcher.SecretResolver) string {
	b.ctx = ctx
	if b.token == "" {
		b.token = "token-1"
	}
	return b.token
}

func (b *capturingBroker) UnregisterCommandToken(token string) {}

func (b *capturingBroker) SocketPath() string { return "/tmp/broker.sock" }

type stubProjectRepo struct {
	projects []*orchestrator.Project
}

func (r stubProjectRepo) CreateProject(project *orchestrator.Project) error { return nil }

func (r stubProjectRepo) GetProject(id string) (*orchestrator.Project, error) {
	for _, project := range r.projects {
		if project != nil && project.ID == id {
			return project, nil
		}
	}
	return nil, fmt.Errorf("project not found: %s", id)
}

func (r stubProjectRepo) ListProjects() ([]*orchestrator.Project, error) {
	return r.projects, nil
}

func (r stubProjectRepo) SetProjectWorkspace(projectID, workspaceID string) error { return nil }

func (r stubProjectRepo) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	return nil, nil
}

func (r stubProjectRepo) DeleteProject(id string) error { return nil }

func TestBrokerRegistry_RegisterBrokerCommands_ResolvesWorkspaceScope(t *testing.T) {
	broker := &capturingBroker{}
	registry := brokerRegistry{
		broker: broker,
		projects: stubProjectRepo{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkDir: "/workspace/proj-1", WorkspaceID: "ws-1"},
			{ID: "proj-2", WorkDir: "/workspace/proj-2", WorkspaceID: "ws-1"},
			{ID: "proj-3", WorkDir: "/workspace/proj-3", WorkspaceID: "ws-2"},
		}},
	}

	resp, err := registry.RegisterBrokerCommands(nil, nil,"proj-1")
	if err != nil {
		t.Fatalf("RegisterBrokerCommands: %v", err)
	}
	if resp.Token != "token-1" {
		t.Fatalf("token = %q, want %q", resp.Token, "token-1")
	}
	if broker.ctx.ProjectID != "proj-1" {
		t.Fatalf("project id = %q, want %q", broker.ctx.ProjectID, "proj-1")
	}
	if broker.ctx.ProjectDir != "/workspace/proj-1" {
		t.Fatalf("project dir = %q, want %q", broker.ctx.ProjectDir, "/workspace/proj-1")
	}
	if broker.ctx.WorkspaceID != "ws-1" {
		t.Fatalf("workspace id = %q, want %q", broker.ctx.WorkspaceID, "ws-1")
	}
	allowed := append([]string(nil), broker.ctx.AllowedProjectIDs...)
	sort.Strings(allowed)
	if !reflect.DeepEqual(allowed, []string{"proj-1", "proj-2"}) {
		t.Fatalf("allowed project ids = %v, want [proj-1 proj-2]", allowed)
	}
}

func TestBrokerRegistry_RegisterBrokerCommands_UnassignedWorkspaceDefaultsToSelf(t *testing.T) {
	broker := &capturingBroker{}
	registry := brokerRegistry{
		broker: broker,
		projects: stubProjectRepo{projects: []*orchestrator.Project{
			{ID: "proj-4", WorkDir: "/workspace/proj-4"},
			{ID: "proj-5", WorkDir: "/workspace/proj-5", WorkspaceID: "ws-9"},
		}},
	}

	resp, err := registry.RegisterBrokerCommands(nil, nil,"proj-4")
	if err != nil {
		t.Fatalf("RegisterBrokerCommands: %v", err)
	}
	if resp.Token != "token-1" {
		t.Fatalf("token = %q, want %q", resp.Token, "token-1")
	}
	if got := broker.ctx.AllowedProjectIDs; !reflect.DeepEqual(got, []string{"proj-4"}) {
		t.Fatalf("allowed project ids = %v, want [proj-4]", got)
	}
}

// gate replay は dispatcher.Job.ExecutionState を頼りに replay 時の task.Status を
// 再現する。toAPIJob / toDispatcherJob のいずれかでこの値が落ちると CompleteJob 経由の
// UpdateJob が空文字で上書きしてしまい、replay が永続的に不可能になる。往復で値が
// 保たれることを保証する回帰テスト。
func TestToAPIJob_PreservesExecutionState(t *testing.T) {
	now := time.Now().UTC()
	src := &dispatcher.Job{
		ID:             "job-1",
		TaskID:         "task-1",
		ProjectID:      "proj-1",
		HandlerID:      "kit/handler",
		Role:           "gate",
		RuntimeID:      "rt-1",
		Interactive:    true,
		TTY:            true,
		Status:         dispatcher.JobStatusCompleted,
		ExitCode:       0,
		Output:         "ok",
		ExecutionState: "verifying",
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	got := toAPIJob(src)
	if got.ExecutionState != "verifying" {
		t.Fatalf("toAPIJob dropped ExecutionState: got %q, want %q", got.ExecutionState, "verifying")
	}
}

func TestToDispatcherJob_PreservesExecutionState(t *testing.T) {
	now := time.Now().UTC()
	src := &api.Job{
		ID:             "job-1",
		TaskID:         "task-1",
		ProjectID:      "proj-1",
		HandlerID:      "kit/handler",
		Role:           "gate",
		RuntimeID:      "rt-1",
		Interactive:    true,
		TTY:            true,
		Status:         api.JobStatusCompleted,
		ExitCode:       0,
		Output:         "ok",
		ExecutionState: "done",
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	got := toDispatcherJob(src)
	if got.ExecutionState != "done" {
		t.Fatalf("toDispatcherJob dropped ExecutionState: got %q, want %q", got.ExecutionState, "done")
	}
}

func TestTranscriptLogReader_StatJobLog(t *testing.T) {
	rootDir := t.TempDir()
	runtimeID := "rt-stat"
	runtimeDir := filepath.Join(rootDir, runtimeID)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	content := "some transcript data\n"
	if err := os.WriteFile(filepath.Join(runtimeDir, "transcript.log"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	r := transcriptLogReader{rootDir: rootDir}
	size, mtime, err := r.StatJobLog(runtimeID)
	if err != nil {
		t.Fatalf("StatJobLog() error = %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("size = %d, want %d", size, len(content))
	}
	if mtime.IsZero() {
		t.Error("mtime should not be zero")
	}
}

func TestTranscriptLogReader_StatJobLog_NotFound(t *testing.T) {
	rootDir := t.TempDir()
	r := transcriptLogReader{rootDir: rootDir}
	_, _, err := r.StatJobLog("nonexistent-rt")
	if !os.IsNotExist(err) {
		t.Errorf("error = %v, want os.ErrNotExist", err)
	}
}
