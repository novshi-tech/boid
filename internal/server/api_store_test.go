package server

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type capturingBroker struct {
	token string
	ctx   dispatcher.BrokerContext
}

func (b *capturingBroker) RegisterCommands(commands map[string]orchestrator.CommandDef, builtinPolicies map[string]sandbox.BuiltinPolicy, ctx dispatcher.BrokerContext, resolve dispatcher.SecretResolver) string {
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
