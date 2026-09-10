package orchestrator

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/db/migrate"
)

func TestDefaultMetaprojects_IdempotentAndReloadable(t *testing.T) {
	d, err := db.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := migrate.Apply(d.Conn); err != nil {
		t.Fatal(err)
	}
	workspaces := NewWorkspaceRepository(d.Conn)
	if err := workspaces.EnsureDefault(); err != nil {
		t.Fatal(err)
	}
	if err := workspaces.Save("team", &WorkspaceMeta{BaseBranch: "${current_branch}", ForkPoint: "origin/main", TaskBehaviors: map[string]TaskBehavior{"executor": {}}}); err != nil {
		t.Fatal(err)
	}
	store := NewProjectStore()
	for i := 0; i < 2; i++ {
		if err := EnsureDefaultMetaprojects(d.Conn, store); err != nil {
			t.Fatal(err)
		}
	}
	projects, err := ListProjects(d.Conn)
	if err != nil || len(projects) != 2 {
		t.Fatalf("projects = %+v: %v", projects, err)
	}
	for _, p := range projects {
		if !IsDefaultMetaproject(p) {
			t.Fatalf("not a managed receiver: %+v", p)
		}
	}
	restarted := NewProjectStore()
	if errs := restarted.LoadAll(projects); len(errs) != 0 {
		t.Fatal(errs)
	}
	wsStore := NewWorkspaceStore(t.TempDir())
	wsStore.SetRepository(workspaces)
	restarted.SetWorkspaceStore(wsStore)
	meta, err := restarted.GetWithWorkspace(context.Background(), DefaultMetaprojectID("team"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.SecretNamespace != "team" || !IsCardProject(meta) || meta.BaseBranch != "" || meta.ForkPoint != "" || len(meta.TaskBehaviors) != 1 {
		t.Fatalf("runtime meta = %+v", meta)
	}
	if !meta.CardCommands["judge"].CardWrite {
		t.Fatal("judge cannot record its decision")
	}
	if err := workspaces.Save("new-team", &WorkspaceMeta{}); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDefaultMetaprojects(d.Conn, restarted); err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.Get(DefaultMetaprojectID("new-team")); !ok {
		t.Fatal("new workspace has no default")
	}
	tx, err := d.Conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	_, _, detached, err := applyWorkspaceProjectAssignments(tx, "team", map[string]string{})
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if len(detached) != 0 {
		t.Fatalf("apply detached the built-in receiver: %v", detached)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	receiver, err := GetProject(d.Conn, DefaultMetaprojectID("team"))
	if err != nil || !IsDefaultMetaproject(receiver) {
		t.Fatalf("receiver after apply: %+v, %v", receiver, err)
	}
	if err := SetProjectWorkspace(d.Conn, receiver.ID, "default"); err == nil {
		t.Fatal("built-in receiver was reassigned")
	}
	if err := workspaces.Remove("team"); err != nil {
		t.Fatal(err)
	}
	if _, err := GetProject(d.Conn, receiver.ID); err == nil {
		t.Fatal("deleted workspace left a broken built-in receiver")
	}

}

func TestDefaultMetaproject_PlansWithoutClone(t *testing.T) {
	p := &Project{ID: DefaultMetaprojectID("team"), WorkspaceID: "team"}
	meta := DefaultMetaprojectMeta("team")
	task := &Task{ID: "judge-task", ProjectID: p.ID, Type: TaskTypeExecution, Exec: &ExecAttrs{Behavior: "judge", Readonly: true}}
	planner := &DispatchPlanner{Meta: stubMetaCache{meta: meta}, Projects: stubProjectCatalog{projects: []*Project{p}}, Tasks: stubTaskLookup{task: task}}
	spec, _, err := planner.PlanHook(&HookFireEvent{ProjectID: p.ID, TaskID: task.ID, Hook: Hook{ID: "judge", Kind: HandlerKindAgent}})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Visibility.Clone != nil || spec.Visibility.ProjectDir != "" {
		t.Fatalf("repository-free judge got clone: %+v", spec.Visibility)
	}
}
