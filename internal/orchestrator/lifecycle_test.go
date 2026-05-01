package orchestrator_test

import (
	"context"
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

type fakeLifecycleStore struct {
	actions []*orchestrator.Action
	err     error
}

func (s *fakeLifecycleStore) ListActionsByTask(_ string) ([]*orchestrator.Action, error) {
	return s.actions, s.err
}

func TestDeriveLifecycle_NilStore(t *testing.T) {
	lc, err := orchestrator.DeriveLifecycle(context.Background(), "t1", nil, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !lc.Executed {
		t.Errorf("expected Executed=true")
	}
	if lc.Abort != nil {
		t.Errorf("expected Abort=nil, got %+v", lc.Abort)
	}
}

func TestDeriveLifecycle_AbortReason(t *testing.T) {
	store := &fakeLifecycleStore{
		actions: []*orchestrator.Action{
			{Type: "start", FromStatus: "pending", ToStatus: "executing"},
			{
				Type:       "abort",
				FromStatus: "executing",
				ToStatus:   "aborted",
				Payload:    []byte(`{"code":"manual_abort","message":"user requested"}`),
			},
		},
	}
	lc, err := orchestrator.DeriveLifecycle(context.Background(), "t1", store, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lc.Abort == nil {
		t.Fatal("expected Abort != nil")
	}
	if lc.Abort.Code != "manual_abort" {
		t.Errorf("expected Abort.Code=manual_abort, got %q", lc.Abort.Code)
	}
	if lc.Abort.Message != "user requested" {
		t.Errorf("expected Abort.Message=%q, got %q", "user requested", lc.Abort.Message)
	}
}
