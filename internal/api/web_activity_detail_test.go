package api

import (
	"errors"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/timeline"
	"strings"
	"testing"
)

func TestCardDetailActivityMatchesList(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	h.CardActivity = orchestrator.NewTaskRepository(repo)
	newCardTimelineTestCard(t, repo, projectID, "card-activity")
	addOpenChild(t, repo, "card-activity", "child", "Draft work")
	_, body := getHTML(t, h, "/tasks/card-activity")
	if !strings.Contains(body, "list-row-activity-work") || !strings.Contains(body, ">Draft</span>") {
		t.Fatal("detail should expose the same Draft work activity as the list")
	}
}

type unavailableCardTimeline struct{ CardTimelineStore }

func (unavailableCardTimeline) CardPinnedItems(string) ([]timeline.CardItem, error) {
	return nil, errors.New("unavailable")
}

func TestCurrentActivityFetchFailureIsNotEmptySuccess(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-activity")
	h.CardTimeline = unavailableCardTimeline{h.CardTimeline}
	code, _ := getHTML(t, h, "/tasks/card-activity/fragment?kind=pinned")
	if code != 500 {
		t.Fatalf("fragment failure = %d, want 500 to retain last known activity", code)
	}
	_, body := getHTML(t, h, "/tasks/card-activity")
	if !strings.Contains(body, "Unable to load current activity") {
		t.Fatal("initial page must explain missing activity")
	}
}

func TestFinishedWorkNeedsDecisionOnlyOnLiveCard(t *testing.T) {
	h, conn, projectID := newCardTimelineTestHandler(t)
	h.CardActivity = orchestrator.NewTaskRepository(conn)
	newCardTimelineTestCard(t, conn, projectID, "card-activity")
	task := &orchestrator.Task{ID: "done-child", ParentID: "card-activity", ProjectID: projectID, Type: orchestrator.TaskTypeExecution, Status: orchestrator.TaskStatusDone, Exec: &orchestrator.ExecAttrs{}}
	if err := orchestrator.CreateTask(conn, task); err != nil {
		t.Fatal(err)
	}
	_, body := getHTML(t, h, "/tasks/card-activity")
	if !strings.Contains(body, "Work finished · Needs decision") {
		t.Fatal("finished child should make remaining decision visible")
	}
	card, err := orchestrator.GetTask(conn, "card-activity")
	if err != nil {
		t.Fatal(err)
	}
	card.Status = orchestrator.TaskStatusDone
	if err := orchestrator.UpdateTask(conn, card); err != nil {
		t.Fatal(err)
	}
	_, body = getHTML(t, h, "/tasks/card-activity")
	if strings.Contains(body, "Work finished · Needs decision") {
		t.Fatal("closed card should not solicit a decision")
	}
}

// The route's first read can succeed while the activity projection's read fails.
type secondDetailReadFails struct {
	WebService
	reads int
}

func (s *secondDetailReadFails) GetTaskDetail(id string) (*TaskDetailView, error) {
	s.reads++
	if s.reads > 1 {
		return nil, errors.New("read failed")
	}
	return s.WebService.GetTaskDetail(id)
}
func TestCurrentActivitySecondaryReadFailurePreservesFragment(t *testing.T) {
	h, repo, projectID := newCardTimelineTestHandler(t)
	newCardTimelineTestCard(t, repo, projectID, "card-activity")
	h.Service = &secondDetailReadFails{WebService: h.Service}
	code, _ := getHTML(t, h, "/tasks/card-activity/fragment?kind=pinned")
	if code != 500 {
		t.Fatalf("secondary read failure = %d, want 500", code)
	}
}
