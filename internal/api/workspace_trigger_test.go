package api

import (
	"context"
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestWorkspaceTriggersSweepOnceWithoutProjectYAML(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	ws := orchestrator.NewWorkspaceRepository(d.Conn)
	if err := ws.Save("team", &orchestrator.WorkspaceMeta{
		Triggers:     []orchestrator.Trigger{{Name: "sweep", Every: "1m", Run: "echo sweep"}},
		Signals:      orchestrator.SignalsConfig{Sources: []orchestrator.SignalSource{{Connector: "demo/poll", Service: "demo", Every: "2m"}}},
		CardCommands: map[string]orchestrator.CardCommand{"review": {Label: "Review", Run: "echo review"}},
		CardEvents:   orchestrator.CardEventsConfig{Command: "review"},
	}); err != nil {
		t.Fatal(err)
	}
	store := orchestrator.NewProjectStore()
	store.SetWorkspaceStore(orchestrator.NewWorkspaceStoreWithRepo(t.TempDir(), ws))
	for _, id := range []string{"a", "b", "c"} {
		if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: id, WorkDir: "/tmp/" + id}); err != nil {
			t.Fatal(err)
		}
		if err := orchestrator.SetProjectWorkspace(d.Conn, id, "team"); err != nil {
			t.Fatal(err)
		}
		store.SetSynthesizedMeta(id, &orchestrator.ProjectMeta{ID: id, Name: id})
		store.SetWorkspaceID(id, "team")
	}
	jobs := newFakeTriggerJobStore()
	exec := &fakeTriggerExecDispatcher{jobs: jobs}
	svc := &TaskWorkflowService{Triggers: orchestrator.NewTaskRepository(d.Conn), Projects: orchestrator.NewProjectRepository(d.Conn), Meta: store, Jobs: jobs, Exec: exec}
	now := time.Now().UTC()
	result, err := svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Fired) != 2 || exec.callCount() != 2 {
		t.Fatalf("fired=%+v calls=%d", result.Fired, exec.callCount())
	}
	for _, fired := range result.Fired {
		if fired.ProjectID != "" || fired.WorkspaceID != "team" {
			t.Fatalf("trigger escaped receiver: %+v", fired)
		}
	}
	meta, err := store.GetWorkspaceMeta(context.Background(), "team")
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Triggers) != 2 || meta.Triggers[1].Name != "signal:demo/poll" || meta.CardCommands["review"].Run != "echo review" || meta.CardEvents.Command != "review" {
		t.Fatalf("receiver: %+v", meta)
	}
	if command, ok, err := store.WorkspaceCardEventCommandInTx(d.Conn, "team"); err != nil || !ok || command != "review" {
		t.Fatalf("event resolution: %q %v", command, ok)
	}
	result, err = svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Fired) != 0 || exec.callCount() != 2 {
		t.Fatalf("repeated in-flight dispatch: %+v", result)
	}
	// A workspace added after startup is evaluated on the next sweep.
	if err := ws.Save("new-team", &orchestrator.WorkspaceMeta{Triggers: []orchestrator.Trigger{{Name: "new", Every: "1m", Run: "echo new"}}}); err != nil {
		t.Fatal(err)
	}
	result, err = svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Fired) != 1 || result.Fired[0].ProjectID != "" || result.Fired[0].WorkspaceID != "new-team" {
		t.Fatalf("new workspace: %+v", result)
	}
}

func TestWorkspaceSignalTriggerLedgerIsScopedAndDebounced(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	ws := orchestrator.NewWorkspaceRepository(d.Conn)
	for _, slug := range []string{"team", "other"} {
		if err := ws.Save(slug, &orchestrator.WorkspaceMeta{Triggers: []orchestrator.Trigger{{Name: "judge", Every: "1m", On: orchestrator.TriggerOnSignals, Run: "echo judge"}}}); err != nil {
			t.Fatal(err)
		}
	}
	meta := orchestrator.NewProjectStore()
	meta.SetWorkspaceStore(orchestrator.NewWorkspaceStoreWithRepo(t.TempDir(), ws))
	tasks := orchestrator.NewTaskRepository(d.Conn)
	jobs := newFakeTriggerJobStore()
	exec := &fakeTriggerExecDispatcher{jobs: jobs}
	svc := &TaskWorkflowService{Triggers: tasks, Projects: orchestrator.NewProjectRepository(d.Conn), Meta: meta, Jobs: jobs, Exec: exec, Signals: tasks}
	now := time.Now().UTC()
	first, err := svc.SweepTriggers(context.Background(), now)
	if err != nil || len(first.Fired) != 0 {
		t.Fatalf("empty inbox: %+v %v", first, err)
	}
	for _, slug := range []string{"team", "other"} {
		if err := orchestrator.IngestSignals(d.Conn, slug, "demo", "demo/poll", []orchestrator.SignalIngestRow{{ID: "signal", OccurredAt: "2026-10-01T00:00:00Z", Identity: "jira:1"}}); err != nil {
			t.Fatal(err)
		}
	}
	first, err = svc.SweepTriggers(context.Background(), now)
	if err != nil || len(first.Fired) != 2 {
		t.Fatalf("pending inboxes: %+v %v", first, err)
	}
	for _, fire := range first.Fired {
		jobs.complete(fire.JobID, 0)
	}
	second, err := svc.SweepTriggers(context.Background(), now.Add(30*time.Second))
	if err != nil || len(second.Fired) != 0 || len(second.Completed) != 2 {
		t.Fatalf("debounce: %+v %v", second, err)
	}
	third, err := svc.SweepTriggers(context.Background(), now.Add(time.Minute))
	if err != nil || len(third.Fired) != 2 {
		t.Fatalf("elapsed: %+v %v", third, err)
	}
	for _, req := range exec.calls {
		if req.ProjectID != "" || (req.WorkspaceID != "team" && req.WorkspaceID != "other") {
			t.Fatalf("workspace exec scope: %+v", req)
		}
	}
}
