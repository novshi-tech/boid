package orchestrator

import (
	"database/sql"
	"fmt"

	"github.com/google/uuid"
	"github.com/novshi-tech/boid/internal/db"
)

// DefaultMetaprojectID is stable across restarts and distinct per workspace.
func DefaultMetaprojectID(workspace string) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("boid:default-metaproject:"+workspace)).String()
}

// IsDefaultMetaproject checks the whole managed identity, not a display name.
func IsDefaultMetaproject(p *Project) bool {
	return p != nil && p.WorkspaceID != "" && p.ID == DefaultMetaprojectID(p.WorkspaceID) && p.WorkDir == "" && p.UpstreamURL == ""
}

// IsCardProject uses the existing automatic-judgment contract. Existing
// metaprojects need no new marker or migration to appear in the creation form.
func IsCardProject(meta *ProjectMeta) bool {
	if meta == nil || meta.CardEvents.Command == "" {
		return false
	}
	_, ok := meta.CardCommands[meta.CardEvents.Command]
	return ok
}

func DefaultMetaprojectMeta(workspace string) *ProjectMeta {
	readonly := true
	return &ProjectMeta{
		ID: DefaultMetaprojectID(workspace), Name: "Default (" + workspace + ")",
		DefaultTaskBehavior: "judge",
		TaskBehaviors: map[string]TaskBehavior{
			"judge": {
				Readonly:           &readonly,
				DefaultInstruction: &Instruction{Message: "Use /boid-card-judge to advance the card supplied by boid card context. Follow /boid-task for task lifecycle and use boid task ask when clarification is needed."},
			},
		},
		CardCommands: map[string]CardCommand{
			"judge": {Label: "Judge", CardWrite: true, Run: "printf '%s\\n' 'title: \"[judge]\"' 'behavior: judge' 'auto_start: true' | boid task create"},
		},
		CardCommandsOrder: []string{"judge"},
		CardEvents:        CardEventsConfig{Command: "judge"},
	}
}

// EnsureDefaultMetaprojects creates repository-free receivers atomically. The
// workspace list is read inside the same transaction as membership creation.
// Called before presenting the Web creation form, including for new workspaces.
func EnsureDefaultMetaprojects(conn *sql.DB, store *ProjectStore) error {
	var defaults []*Project
	err := db.InTxDB(conn, func(tx db.DBTX) error {
		workspaces, err := ListWorkspaces(tx)
		if err != nil {
			return err
		}
		projects, err := ListProjects(tx)
		if err != nil {
			return err
		}
		byID := make(map[string]*Project, len(projects))
		for _, p := range projects {
			byID[p.ID] = p
		}
		for _, ws := range workspaces {
			id := DefaultMetaprojectID(ws.ID)
			p := byID[id]
			if p == nil {
				p = &Project{ID: id, WorkspaceID: ws.ID}
				if err := CreateProject(tx, p); err != nil {
					return err
				}
				if err := SetProjectWorkspace(tx, id, ws.ID); err != nil {
					return err
				}
			}
			if !IsDefaultMetaproject(p) {
				return fmt.Errorf("default metaproject identity %q is already in use", id)
			}
			defaults = append(defaults, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, p := range defaults {
		store.SetSynthesizedMeta(p.ID, DefaultMetaprojectMeta(p.WorkspaceID))
		store.SetWorkspaceID(p.ID, p.WorkspaceID)
	}
	return nil
}
