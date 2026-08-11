package orchestrator_test

import (
	"testing"
	"time"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestWorkspaceWatchdog_UpsertAndGet_RoundTrips(t *testing.T) {
	d := createTestProject(t)
	if err := orchestrator.NewWorkspaceRepository(d.Conn).Create("ws-a", &orchestrator.WorkspaceMeta{}); err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	if err := orchestrator.TouchWorkspaceIngestSuccess(d.Conn, "ws-a", now); err != nil {
		t.Fatalf("touch ingest success: %v", err)
	}

	got, err := orchestrator.GetWorkspaceWatchdog(d.Conn, "ws-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastIngestSuccessAt == nil || !got.LastIngestSuccessAt.Equal(now) {
		t.Errorf("LastIngestSuccessAt = %v, want %v", got.LastIngestSuccessAt, now)
	}
	if got.LastTriageReviewAt != nil {
		t.Errorf("LastTriageReviewAt should still be unset, got %v", got.LastTriageReviewAt)
	}

	later := now.Add(time.Hour)
	if err := orchestrator.TouchWorkspaceTriageReview(d.Conn, "ws-a", later); err != nil {
		t.Fatalf("touch triage review: %v", err)
	}
	got, err = orchestrator.GetWorkspaceWatchdog(d.Conn, "ws-a")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastIngestSuccessAt == nil || !got.LastIngestSuccessAt.Equal(now) {
		t.Errorf("LastIngestSuccessAt should be preserved by a later TouchTriageReview call, got %v", got.LastIngestSuccessAt)
	}
	if got.LastTriageReviewAt == nil || !got.LastTriageReviewAt.Equal(later) {
		t.Errorf("LastTriageReviewAt = %v, want %v", got.LastTriageReviewAt, later)
	}
}

func TestGetWorkspaceWatchdog_NoRow_ReturnsZeroValue(t *testing.T) {
	d := createTestProject(t)
	got, err := orchestrator.GetWorkspaceWatchdog(d.Conn, "never-touched")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("expected a non-nil zero-value watchdog for a workspace with no recorded activity")
	}
	if got.LastIngestSuccessAt != nil || got.LastTriageReviewAt != nil {
		t.Errorf("expected both timestamps unset, got %+v", got)
	}
}

func TestWatchdogGuidance_ThresholdBreach(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	th := orchestrator.WatchdogThresholds{
		IngestStale: 48 * time.Hour,
		ReviewStale: 14 * 24 * time.Hour,
	}

	// Never recorded at all: both are "silence since forever" — guidance for both.
	msgs := orchestrator.WatchdogGuidance(now, "ws-a", &orchestrator.WorkspaceWatchdog{}, th)
	if len(msgs) != 2 {
		t.Fatalf("never-recorded watchdog: got %d guidance items, want 2: %v", len(msgs), msgs)
	}

	// Within threshold: no guidance.
	recentIngest := now.Add(-time.Hour)
	recentReview := now.Add(-24 * time.Hour)
	msgs = orchestrator.WatchdogGuidance(now, "ws-a", &orchestrator.WorkspaceWatchdog{
		LastIngestSuccessAt: &recentIngest,
		LastTriageReviewAt:  &recentReview,
	}, th)
	if len(msgs) != 0 {
		t.Errorf("recent activity: expected no guidance, got %v", msgs)
	}

	// Ingest stale only.
	staleIngest := now.Add(-72 * time.Hour)
	msgs = orchestrator.WatchdogGuidance(now, "ws-a", &orchestrator.WorkspaceWatchdog{
		LastIngestSuccessAt: &staleIngest,
		LastTriageReviewAt:  &recentReview,
	}, th)
	if len(msgs) != 1 {
		t.Fatalf("stale ingest only: got %d guidance items, want 1: %v", len(msgs), msgs)
	}

	// Review stale only.
	staleReview := now.Add(-20 * 24 * time.Hour)
	msgs = orchestrator.WatchdogGuidance(now, "ws-a", &orchestrator.WorkspaceWatchdog{
		LastIngestSuccessAt: &recentIngest,
		LastTriageReviewAt:  &staleReview,
	}, th)
	if len(msgs) != 1 {
		t.Fatalf("stale review only: got %d guidance items, want 1: %v", len(msgs), msgs)
	}
}
