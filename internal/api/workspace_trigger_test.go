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
	svc := &TaskWorkflowService{Triggers: orchestrator.NewTaskRepository(d.Conn), Projects: orchestrator.NewProjectRepository(d.Conn), Meta: store, Jobs: jobs, Exec: exec,
		EnsureTriggerProjects: func() error { return orchestrator.EnsureDefaultMetaprojects(d.Conn, store) },
	}
	now := time.Now().UTC()
	result, err := svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	receiver := orchestrator.DefaultMetaprojectID("team")
	if len(result.Fired) != 2 || exec.callCount() != 2 {
		t.Fatalf("fired=%+v calls=%d", result.Fired, exec.callCount())
	}
	for _, fired := range result.Fired {
		if fired.ProjectID != receiver {
			t.Fatalf("trigger escaped receiver: %+v", fired)
		}
	}
	meta, err := store.GetWithWorkspace(context.Background(), receiver)
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Triggers) != 2 || meta.Triggers[1].Name != "signal:demo/poll" || meta.CardCommands["review"].Run != "echo review" || meta.CardEvents.Command != "review" {
		t.Fatalf("receiver: %+v", meta)
	}
	if command, ok := store.CardEventCommand(receiver); !ok || command != "review" {
		t.Fatalf("event resolution: %q %v", command, ok)
	}
	result, err = svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Fired) != 0 || exec.callCount() != 2 {
		t.Fatalf("repeated in-flight dispatch: %+v", result)
	}
	// A workspace added after startup gets its receiver on the next sweep.
	if err := ws.Save("new-team", &orchestrator.WorkspaceMeta{Triggers: []orchestrator.Trigger{{Name: "new", Every: "1m", Run: "echo new"}}}); err != nil {
		t.Fatal(err)
	}
	result, err = svc.SweepTriggers(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Fired) != 1 || result.Fired[0].ProjectID != orchestrator.DefaultMetaprojectID("new-team") {
		t.Fatalf("new workspace: %+v", result)
	}
}
