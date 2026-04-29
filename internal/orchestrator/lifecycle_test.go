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
	if lc.ReworkCount != 0 {
		t.Errorf("expected ReworkCount=0, got %d", lc.ReworkCount)
	}
	if lc.Abort != nil {
		t.Errorf("expected Abort=nil, got %+v", lc.Abort)
	}
}

func TestDeriveLifecycle_ReworkCount_OnlyCountsTransitionsIntoReworking(t *testing.T) {
	// 3 つの実 rework サイクルを再現する: 各サイクルは
	//   auto_advance (executing|reworking → reworking)
	//   hook_fired   (reworking → reworking)
	//   exit_gate_fired (reworking → reworking)
	// で構成される。rework_count は遷移のみ数えるべきで、
	// hook_fired / exit_gate_fired の self-loop は数えない。
	store := &fakeLifecycleStore{
		actions: []*orchestrator.Action{
			{Type: "start", FromStatus: "pending", ToStatus: "executing"},
			{Type: "hook_fired", FromStatus: "executing", ToStatus: "executing"},
			{Type: "exit_gate_fired", FromStatus: "executing", ToStatus: "executing"},
			// 1 回目の rework サイクル
			{Type: "auto_advance", FromStatus: "executing", ToStatus: "reworking"},
			{Type: "hook_fired", FromStatus: "reworking", ToStatus: "reworking"},
			{Type: "exit_gate_fired", FromStatus: "reworking", ToStatus: "reworking"},
			// 2 回目の rework サイクル
			{Type: "auto_advance", FromStatus: "reworking", ToStatus: "reworking"},
			{Type: "hook_fired", FromStatus: "reworking", ToStatus: "reworking"},
			{Type: "exit_gate_fired", FromStatus: "reworking", ToStatus: "reworking"},
			// 3 回目の rework サイクル
			{Type: "auto_advance", FromStatus: "reworking", ToStatus: "reworking"},
			{Type: "hook_fired", FromStatus: "reworking", ToStatus: "reworking"},
			{Type: "exit_gate_fired", FromStatus: "reworking", ToStatus: "reworking"},
		},
	}
	lc, err := orchestrator.DeriveLifecycle(context.Background(), "t1", store, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// auto_advance: executing→reworking のみ +1。
	// auto_advance: reworking→reworking は self-loop なので数えない。
	// hook_fired / exit_gate_fired (reworking→reworking) も数えない。
	if lc.ReworkCount != 1 {
		t.Errorf("expected ReworkCount=1 (only the executing→reworking transition), got %d", lc.ReworkCount)
	}
}

func TestDeriveLifecycle_ReworkCount_MultipleReentries(t *testing.T) {
	// reopen → reworking → verifying → reworking のように
	// 別状態を経由して rework に再入場した場合は別カウント。
	store := &fakeLifecycleStore{
		actions: []*orchestrator.Action{
			{Type: "auto_advance", FromStatus: "executing", ToStatus: "reworking"},      // +1
			{Type: "auto_advance", FromStatus: "reworking", ToStatus: "verifying"},      // 0
			{Type: "auto_advance", FromStatus: "verifying", ToStatus: "reworking"},      // +1
			{Type: "reopen", FromStatus: "done", ToStatus: "reworking"},                 // +1 (manual reopen also counts)
			{Type: "hook_fired", FromStatus: "reworking", ToStatus: "reworking"},        // 0 (self-loop)
		},
	}
	lc, err := orchestrator.DeriveLifecycle(context.Background(), "t1", store, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lc.ReworkCount != 3 {
		t.Errorf("expected ReworkCount=3, got %d", lc.ReworkCount)
	}
}

func TestDeriveLifecycle_AbortReason(t *testing.T) {
	store := &fakeLifecycleStore{
		actions: []*orchestrator.Action{
			{Type: "auto_advance", FromStatus: "executing", ToStatus: "reworking"},
			{
				Type:       "auto_advance",
				FromStatus: "reworking",
				ToStatus:   "aborted",
				Payload:    []byte(`{"code":"rework_limit_exceeded"}`),
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
	if lc.Abort.Code != "rework_limit_exceeded" {
		t.Errorf("expected Abort.Code=rework_limit_exceeded, got %q", lc.Abort.Code)
	}
}
