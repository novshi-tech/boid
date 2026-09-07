package api

import (
	"github.com/novshi-tech/boid/internal/db"
	"github.com/novshi-tech/boid/internal/timeline"
)

// DBCardTimelineStore adapts a raw db.DBTX connection to CardTimelineStore
// by calling internal/timeline's read model functions directly — the
// production wiring for WebHandler.CardTimeline, and reused by tests that
// want a real (in-memory SQLite) timeline read model instead of a stub.
type DBCardTimelineStore struct {
	DB db.DBTX
}

func (s DBCardTimelineStore) BuildCardTimeline(cardID, cursor string, limit int) (*timeline.CardTimelinePage, error) {
	return timeline.BuildCardTimeline(s.DB, cardID, cursor, limit)
}

func (s DBCardTimelineStore) CardPinnedItems(cardID string) ([]timeline.CardItem, error) {
	return timeline.CardPinnedItems(s.DB, cardID)
}
