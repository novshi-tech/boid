package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/api"
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

// newSecretTestDB returns a fresh in-memory SQLite with migrations applied,
// scoped to the test. We can't use testutil.NewTestDB here because
// testutil imports internal/server (testutil/server.go), which would create an
// import cycle when used from inside the server package.
func newSecretTestDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

type capturingBroker struct {
	token    string
	ctx      sandbox.TokenContext
	resolver dispatcher.SecretResolver
}

func (b *capturingBroker) RegisterCommands(commands map[string]orchestrator.CommandDef, builtinPolicies map[string]sandbox.BuiltinPolicy, ctx sandbox.TokenContext, resolve dispatcher.SecretResolver) string {
	b.ctx = ctx
	b.resolver = resolve
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

// WorkspaceExists always reports true: these fixtures exercise broker
// registration / project resolution, never ProjectAppService.SetProjectWorkspace's
// MAJOR 5 existence check, so a permissive stub keeps them unaffected.
func (r stubProjectRepo) WorkspaceExists(slug string) (bool, error) { return true, nil }

func (r stubProjectRepo) ListWorkspaces() ([]*orchestrator.WorkspaceSummary, error) {
	return nil, nil
}

func (r stubProjectRepo) DeleteProject(id string) error { return nil }

func (r stubProjectRepo) SetProjectUpstreamURL(projectID, upstreamURL string) error { return nil }

// stubMetaResolver returns a hydrated ProjectMeta whose SecretNamespace mirrors
// the project's workspace id — same contract as orchestrator.ProjectStore's
// GetWithWorkspace, simplified for the broker register tests.
type stubMetaResolver struct {
	namespaces map[string]string // project_id → SecretNamespace
}

func (r stubMetaResolver) GetWithWorkspace(_ context.Context, projectID string) (*orchestrator.ProjectMeta, error) {
	ns, ok := r.namespaces[projectID]
	if !ok {
		return nil, fmt.Errorf("project %q: meta not loaded", projectID)
	}
	return &orchestrator.ProjectMeta{SecretNamespace: ns}, nil
}

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

// Regression: `/api/broker/register` (the path `boid exec` takes) used to
// hard-code namespace="default" when resolving secret: env values, so any
// project whose secrets live in a workspace namespace would see host_commands
// rejected with "required secret(s) unavailable" even after `boid secret set
// --namespace <workspace>`. The dispatcher path in
// internal/dispatcher/runner.go already routed via spec.SecretNamespace; only
// this broker register path was missing the hydration.
func TestBrokerRegistry_RegisterBrokerCommands_ResolvesSecretInWorkspaceNamespace(t *testing.T) {
	d := newSecretTestDB(t)
	secrets, err := dispatcher.NewSecretStore(d.Conn, dispatcher.GenerateKey())
	if err != nil {
		t.Fatalf("NewSecretStore: %v", err)
	}
	// Same key in two namespaces so a wrong-namespace lookup would silently
	// return the "default" value instead of failing — we want to assert that
	// resolution explicitly picks the workspace value.
	if err := secrets.Set("ws-1", "MY_TOKEN", "ws-value"); err != nil {
		t.Fatalf("Set ws-1: %v", err)
	}
	if err := secrets.Set("default", "MY_TOKEN", "default-value"); err != nil {
		t.Fatalf("Set default: %v", err)
	}

	broker := &capturingBroker{}
	registry := brokerRegistry{
		broker: broker,
		projects: stubProjectRepo{projects: []*orchestrator.Project{
			{ID: "proj-1", WorkDir: "/workspace/proj-1", WorkspaceID: "ws-1"},
		}},
		metaStore:   stubMetaResolver{namespaces: map[string]string{"proj-1": "ws-1"}},
		secretStore: secrets,
	}

	if _, err := registry.RegisterBrokerCommands(nil, nil, "proj-1"); err != nil {
		t.Fatalf("RegisterBrokerCommands: %v", err)
	}
	if broker.resolver == nil {
		t.Fatal("resolver must be set when secretStore is configured")
	}
	got, err := broker.resolver("MY_TOKEN")
	if err != nil {
		t.Fatalf("resolver(MY_TOKEN): %v", err)
	}
	if got != "ws-value" {
		t.Fatalf("resolver returned %q (default namespace), want %q (workspace ws-1)", got, "ws-value")
	}
}

// Project with no workspace assignment should fall back to the "default"
// namespace so legacy / unlinked projects keep working.
func TestBrokerRegistry_RegisterBrokerCommands_FallsBackToDefaultWhenSecretNamespaceEmpty(t *testing.T) {
	d := newSecretTestDB(t)
	secrets, err := dispatcher.NewSecretStore(d.Conn, dispatcher.GenerateKey())
	if err != nil {
		t.Fatalf("NewSecretStore: %v", err)
	}
	if err := secrets.Set("default", "MY_TOKEN", "default-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	broker := &capturingBroker{}
	registry := brokerRegistry{
		broker: broker,
		projects: stubProjectRepo{projects: []*orchestrator.Project{
			{ID: "proj-x", WorkDir: "/workspace/proj-x"},
		}},
		metaStore:   stubMetaResolver{namespaces: map[string]string{"proj-x": ""}},
		secretStore: secrets,
	}

	if _, err := registry.RegisterBrokerCommands(nil, nil, "proj-x"); err != nil {
		t.Fatalf("RegisterBrokerCommands: %v", err)
	}
	got, err := broker.resolver("MY_TOKEN")
	if err != nil {
		t.Fatalf("resolver(MY_TOKEN): %v", err)
	}
	if got != "default-value" {
		t.Fatalf("resolver returned %q, want %q (default namespace fallback)", got, "default-value")
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
