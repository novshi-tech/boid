package api

import (
	"github.com/novshi-tech/boid/internal/orchestrator"
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
