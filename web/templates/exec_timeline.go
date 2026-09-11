package templates

import (
	"sort"
	"time"

	"github.com/novshi-tech/boid/internal/timeline"
)

// execTimelineRow is one entry in the shared newest-first history.
// Child terminal events retain their recorded status even after a reopen.
type execTimelineRow struct {
	Group   *timeline.StatusGroup
	Event   *timeline.Event
	Child   *execChildHistoryEvent
	Time    time.Time
	HasTime bool
	Sticky  bool
}

func execTimelineRows(groups []timeline.StatusGroup, children []ChildTreeNode) []execTimelineRow {
	var rows []execTimelineRow
	for _, group := range groups {
		rows = append(rows, execTimelineRow{Group: &group, Time: group.EnteredAt, HasTime: group.HasEnteredAt})
		for _, event := range group.Events {
			rows = append(rows, execTimelineRow{Event: &event, Time: event.Time, HasTime: event.HasTime, Sticky: event.Sticky})
		}
	}
	for _, child := range execChildHistoryEvents(children) {
		rows = append(rows, execTimelineRow{Child: &child, Time: child.Time, HasTime: child.HasTime})
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Sticky != rows[j].Sticky {
			return rows[i].Sticky
		}
		if rows[i].HasTime != rows[j].HasTime {
			return rows[i].HasTime
		}
		return rows[i].Time.After(rows[j].Time)
	})
	return rows
}
