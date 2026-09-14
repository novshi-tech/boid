package orchestrator_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/testutil"
)

func TestCardExecutionStates_SlotLifecycle(t *testing.T) {
	for _, command := range []string{orchestrator.CardRequestCommandKeyGo, "discuss"} {
		t.Run(command, func(t *testing.T) {
			d := testutil.NewTestDB(t)
			card := newTestCard(t, d, "proj-1", "card-1")
			read := func() orchestrator.CardExecutionState {
				t.Helper()
				states, err := orchestrator.CardExecutionStatesByIDs(d.Conn, []string{card, card, "missing", ""})
				if err != nil {
					t.Fatal(err)
				}
				if len(states) != 1 {
					t.Fatalf("unexpected cards: %+v", states)
				}
				return states[card]
			}
			queued := &orchestrator.CardRequest{CardID: card, CommandKey: command}
			if err := orchestrator.CreateCardRequest(d.Conn, queued); err != nil {
				t.Fatal(err)
			}
			if read().Occupied {
				t.Fatal("queued request must not occupy a slot")
			}
			active := &orchestrator.CardRequest{CardID: card, CommandKey: command, Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher", Launched: orchestrator.CardRequestDefinition{Label: "Discuss"}}
			if err := orchestrator.CreateCardRequest(d.Conn, active); err != nil {
				t.Fatal(err)
			}
			state := read()
			if !state.Occupied || state.NeedsInput {
				t.Fatalf("launching: %+v", state)
			}
			if command == "discuss" && state.CommandLabel != "Discuss" {
				t.Fatalf("command label: %+v", state)
			}
			if command == orchestrator.CardRequestCommandKeyGo && state.CommandLabel != "" {
				t.Fatalf("Go label: %+v", state)
			}
			if err := orchestrator.AttachCardRequest(d.Conn, active.ID, orchestrator.CardRequestTargetKindSession, "session-job"); err != nil {
				t.Fatal(err)
			}
			if !read().Occupied {
				t.Fatal("attached session must occupy a slot")
			}
			if err := orchestrator.FinishCardRequest(d.Conn, active.ID, "done"); err != nil {
				t.Fatal(err)
			}
			if state := read(); state.Occupied || state.CommandLabel != "" {
				t.Fatalf("released slot with queued successor: %+v", state)
			}
		})
	}
}

func TestCardExecutionStates_AllDescendantsAndCommandTarget(t *testing.T) {
	d := testutil.NewTestDB(t)
	card := newTestCard(t, d, "proj-1", "card-1")
	other := newTestCard(t, d, "proj-1", "card-2")
	create := func(id, parent string, status orchestrator.TaskStatus) {
		t.Helper()
		if err := orchestrator.CreateTask(d.Conn, &orchestrator.Task{ID: id, ParentID: parent, ProjectID: "proj-1", Type: orchestrator.TaskTypeExecution, Status: status, Exec: &orchestrator.ExecAttrs{}}); err != nil {
			t.Fatal(err)
		}
	}
	// Traverse terminal intermediates as well as executing tasks.
	create("child", card, orchestrator.TaskStatusDone)
	create("grandchild", "child", orchestrator.TaskStatusExecuting)
	create("great-grandchild", "grandchild", orchestrator.TaskStatusAwaiting)
	create("unrelated", other, orchestrator.TaskStatusExecuting)
	read := func() map[string]orchestrator.CardExecutionState {
		t.Helper()
		states, err := orchestrator.CardExecutionStatesByIDs(d.Conn, []string{card, other})
		if err != nil {
			t.Fatal(err)
		}
		return states
	}
	states := read()
	if !states[card].NeedsInput || states[card].Occupied || states[other].NeedsInput || states[other].Occupied {
		t.Fatalf("descendant state and isolation: %+v", states)
	}
	if _, err := d.Conn.Exec(`UPDATE tasks SET status = 'executing' WHERE id = 'great-grandchild'`); err != nil {
		t.Fatal(err)
	}
	if read()[card].NeedsInput {
		t.Fatal("answered descendant must clear input state on next read")
	}
	// The command target need not have a parent_id linking it to the card.
	create("dialogue", "", orchestrator.TaskStatusExecuting)
	create("dialogue-child", "dialogue", orchestrator.TaskStatusAwaiting)
	req := &orchestrator.CardRequest{CardID: card, CommandKey: "discuss", Status: orchestrator.CardRequestStatusLaunching, LauncherJobID: "launcher"}
	if err := orchestrator.CreateCardRequest(d.Conn, req); err != nil {
		t.Fatal(err)
	}
	if err := orchestrator.AttachCardRequest(d.Conn, req.ID, orchestrator.CardRequestTargetKindTask, "dialogue"); err != nil {
		t.Fatal(err)
	}
	if state := read()[card]; !state.Occupied || !state.NeedsInput || state.CommandLabel != "discuss" {
		t.Fatalf("command target descendants: %+v", state)
	}
	// A malformed cycle reachable through a slot must terminate.
	if _, err := d.Conn.Exec(`UPDATE tasks SET parent_id = 'dialogue-child' WHERE id = 'dialogue'`); err != nil {
		t.Fatal(err)
	}
	if !read()[card].NeedsInput {
		t.Fatal("cycle traversal lost awaiting task")
	}
}

func TestCardExecutionStates_Empty(t *testing.T) {
	states, err := orchestrator.CardExecutionStatesByIDs(nil, nil)
	if err != nil || len(states) != 0 {
		t.Fatalf("empty input: %+v, %v", states, err)
	}
}
