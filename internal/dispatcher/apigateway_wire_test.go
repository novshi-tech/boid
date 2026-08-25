package dispatcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/novshi-tech/boid/internal/apigateway"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

// --- resolveEnabledAPIServices ---

func TestResolveEnabledAPIServices_NoWorkspaceReturnsFloorOnly(t *testing.T) {
	r := &Runner{APIGatewayServicesFloor: []string{"myapp"}}
	got := r.resolveEnabledAPIServices("")
	if len(got) != 1 || got[0] != "myapp" {
		t.Errorf("resolveEnabledAPIServices(\"\") = %v, want [myapp]", got)
	}
}

func TestResolveEnabledAPIServices_NilWorkspacesReturnsFloorOnly(t *testing.T) {
	r := &Runner{APIGatewayServicesFloor: []string{"myapp"}}
	got := r.resolveEnabledAPIServices("team-a")
	if len(got) != 1 || got[0] != "myapp" {
		t.Errorf("resolveEnabledAPIServices with nil Workspaces = %v, want [myapp]", got)
	}
}

func TestResolveEnabledAPIServices_FloorPlusWorkspaceAdditions(t *testing.T) {
	r := &Runner{
		APIGatewayServicesFloor: []string{"myapp"},
		Workspaces: fakeWorkspaceLookup{metas: map[string]*orchestrator.WorkspaceMeta{
			"team-a": {Services: []string{"myapp-ops", "bitbucket-api"}},
		}},
	}
	got := r.resolveEnabledAPIServices("team-a")
	want := []string{"myapp", "myapp-ops", "bitbucket-api"}
	if !equalStringSliceDispatcher(got, want) {
		t.Errorf("resolveEnabledAPIServices = %v, want %v", got, want)
	}
}

func TestResolveEnabledAPIServices_WorkspaceLoadNotExistFallsBackToFloor(t *testing.T) {
	r := &Runner{
		APIGatewayServicesFloor: []string{"myapp"},
		Workspaces:              fakeWorkspaceLookup{err: fmt.Errorf("wrap: %w", os.ErrNotExist)},
	}
	got := r.resolveEnabledAPIServices("team-a")
	if !equalStringSliceDispatcher(got, []string{"myapp"}) {
		t.Errorf("resolveEnabledAPIServices (ErrNotExist) = %v, want [myapp]", got)
	}
}

func TestResolveEnabledAPIServices_WorkspaceLoadOtherErrorFallsBackToFloor(t *testing.T) {
	r := &Runner{
		APIGatewayServicesFloor: []string{"myapp"},
		Workspaces:              fakeWorkspaceLookup{err: errors.New("disk on fire")},
	}
	got := r.resolveEnabledAPIServices("team-a")
	if !equalStringSliceDispatcher(got, []string{"myapp"}) {
		t.Errorf("resolveEnabledAPIServices (generic error) = %v, want [myapp] (fail-soft)", got)
	}
}

func equalStringSliceDispatcher(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- Dispatch-level registration/unregistration ---

// TestDispatch_RegistersAndUnregistersAPIGatewayToken mirrors
// TestDispatch_RegistersAndUnregistersGatewayToken (git gateway) for the API
// gateway: a live Dispatch call registers a real apigateway.Registry entry
// carrying the resolved service set, namespace, task id and readonly flag,
// and UnregisterJob revokes it.
func TestDispatch_RegistersAndUnregistersAPIGatewayToken(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "proj-1", WorkDir: "/tmp",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	task := &orchestrator.Task{ProjectID: "proj-1", Title: "Task", Type: orchestrator.TaskTypeExecution, Exec: &orchestrator.ExecAttrs{Behavior: "dev"}}
	if err := orchestrator.CreateTask(d.Conn, task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	gwURL := "https://boid-gateway:9443"
	registry := apigateway.NewRegistry()
	r := &Runner{
		DB:                      d.Conn,
		Projects:                orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:                 &gwFakeBackend{},
		BoidBinary:              "/boid",
		APIGateway:              registry,
		GatewayURL:              &gwURL,
		APIGatewayServicesFloor: []string{"myapp"},
	}

	spec := &orchestrator.JobSpec{
		TaskID:     task.ID,
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "hi"},
		Kind:       orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{Writable: true},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	token, ok := r.apiGatewayTokens[jobID]
	if !ok || token == "" {
		t.Fatalf("apiGatewayTokens[%q] not registered (rtInfo.APIGatewayJobToken wiring likely dropped)", jobID)
	}

	entry, valid := registry.Lookup(token)
	if !valid {
		t.Fatal("registry.Lookup: token registered by Dispatch was not found in the real Registry")
	}
	if !entry.Services["myapp"] {
		t.Errorf("entry.Services = %v, want myapp enabled (from APIGatewayServicesFloor)", entry.Services)
	}
	if entry.TaskID != task.ID {
		t.Errorf("entry.TaskID = %q, want %q", entry.TaskID, task.ID)
	}
	if entry.ReadOnly {
		t.Error("entry.ReadOnly = true, want false (spec.Visibility.Writable is true)")
	}

	r.UnregisterJob(jobID)

	if _, stillValid := registry.Lookup(token); stillValid {
		t.Fatal("token should be revoked from the Registry after UnregisterJob")
	}
	if _, tracked := r.apiGatewayTokens[jobID]; tracked {
		t.Fatal("apiGatewayTokens entry should be removed after UnregisterJob")
	}
}

// TestDispatch_APIGatewayToken_ReadOnlyFromVisibility pins the task.readonly
// → Entry.ReadOnly mapping (docs/plans/api-gateway.md 前提となる決定事項):
// a non-writable job gets a read-only API gateway token.
func TestDispatch_APIGatewayToken_ReadOnlyFromVisibility(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "proj-1", WorkDir: "/tmp",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	gwURL := "https://boid-gateway:9443"
	registry := apigateway.NewRegistry()
	r := &Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:    &gwFakeBackend{},
		BoidBinary: "/boid",
		APIGateway: registry,
		GatewayURL: &gwURL,
	}

	spec := &orchestrator.JobSpec{
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "hi"},
		Kind:       orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{Writable: false},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	token := r.apiGatewayTokens[jobID]
	entry, _ := registry.Lookup(token)
	if !entry.ReadOnly {
		t.Error("entry.ReadOnly = false, want true (spec.Visibility.Writable is false)")
	}
}

// TestDispatch_RegistersAPIGatewayTokenWithSecretNamespace mirrors
// TestDispatch_RegistersGatewayTokenWithSecretNamespace (git gateway,
// gitgateway_wire_test.go) for the API gateway — the same wiring-seams.md
// #11-shaped concern applied to the sibling gateway: it proves
// registerAPIGatewayToken passes spec.SecretNamespace through to
// apigateway.Registry.Register (internal/dispatcher/apigateway_wire.go's
// r.APIGateway.Register(services, spec.SecretNamespace, spec.TaskID,
// readOnly) call), so the real Registry entry a live Dispatch creates
// carries the namespace Server.ServeHTTP will later read back out via
// Lookup to scope credential resolution (see
// TestServer_RoutesCredentialsByTokenNamespace, internal/apigateway/
// server_test.go, for the remaining End B→C→D hop through a real Server).
func TestDispatch_RegistersAPIGatewayTokenWithSecretNamespace(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{
		ID: "proj-1", WorkDir: "/tmp",
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	gwURL := "https://boid-gateway:9443"
	registry := apigateway.NewRegistry()
	r := &Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:    &gwFakeBackend{},
		BoidBinary: "/boid",
		APIGateway: registry,
		GatewayURL: &gwURL,
	}

	spec := &orchestrator.JobSpec{
		ProjectID:       "proj-1",
		Argv:            []string{"echo", "hi"},
		Kind:            orchestrator.JobKindHook,
		Visibility:      orchestrator.Visibility{Writable: true},
		SecretNamespace: "ws-scoped-secret",
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	token, ok := r.apiGatewayTokens[jobID]
	if !ok || token == "" {
		t.Fatalf("apiGatewayTokens[%q] not registered", jobID)
	}

	entry, valid := registry.Lookup(token)
	if !valid {
		t.Fatal("registry.Lookup: token registered by Dispatch was not found in the real Registry")
	}
	if entry.Namespace != "ws-scoped-secret" {
		t.Errorf("entry.Namespace = %q, want %q (spec.SecretNamespace should have been threaded through Register)", entry.Namespace, "ws-scoped-secret")
	}
}

// TestDispatch_APIGatewayUnwired_NoTokenNoPanic verifies the nil-APIGateway
// path (test wiring / a daemon build without the API gateway constructed)
// leaves SandboxRuntimeInfo's API gateway fields empty and Dispatch/
// UnregisterJob do not panic.
func TestDispatch_APIGatewayUnwired_NoTokenNoPanic(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	r := &Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:    &gwFakeBackend{},
		BoidBinary: "/boid",
	}

	spec := &orchestrator.JobSpec{
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "hi"},
		Kind:       orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{Writable: true},
	}

	jobID, err := r.Dispatch(context.Background(), spec, nil)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(r.apiGatewayTokens) != 0 {
		t.Errorf("apiGatewayTokens = %v, want empty when APIGateway is unwired", r.apiGatewayTokens)
	}
	r.UnregisterJob(jobID) // must not panic
}

// TestDispatch_RuntimeStartError_UnregistersAPIGatewayToken mirrors
// TestDispatch_RuntimeStartError_UnregistersBrokerAndGatewayTokens for the
// API gateway: a Runtime.Start failure must not leak the API gateway token
// registered earlier in Dispatch.
func TestDispatch_RuntimeStartError_UnregistersAPIGatewayToken(t *testing.T) {
	d := newGatewayTestDB(t)
	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	gwURL := "https://boid-gateway:9443"
	registry := apigateway.NewRegistry()
	r := &Runner{
		DB:         d.Conn,
		Projects:   orchestrator.DBProjectCatalog{DB: d.Conn},
		Backend:    &gwFakeBackend{launchErr: fmt.Errorf("boom: runtime start failed")},
		BoidBinary: "/boid",
		APIGateway: registry,
		GatewayURL: &gwURL,
	}

	spec := &orchestrator.JobSpec{
		ProjectID:  "proj-1",
		Argv:       []string{"echo", "hi"},
		Kind:       orchestrator.JobKindHook,
		Visibility: orchestrator.Visibility{Writable: true},
	}

	_, err := r.Dispatch(context.Background(), spec, nil)
	if err == nil {
		t.Fatal("expected Dispatch to return an error when Runtime.Start fails")
	}
	if len(r.apiGatewayTokens) != 0 {
		t.Errorf("apiGatewayTokens leaked after Dispatch failure: %#v", r.apiGatewayTokens)
	}
}
