package dispatcher

import (
	"testing"

	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestToDispatchPlan_Interactive_Propagated(t *testing.T) {
	request := &orchestrator.DispatchRequest{
		TaskID:      "task-1",
		ProjectID:   "proj-1",
		HandlerID:   "hook-1",
		Role:        orchestrator.RoleHook,
		Interactive: true,
	}
	plan := toDispatchPlan(request)
	if !plan.Interactive {
		t.Fatal("expected Interactive=true in DispatchPlan, got false")
	}
}

func TestToDispatchPlan_Interactive_False(t *testing.T) {
	request := &orchestrator.DispatchRequest{
		TaskID:      "task-1",
		ProjectID:   "proj-1",
		HandlerID:   "hook-1",
		Role:        orchestrator.RoleHook,
		Interactive: false,
	}
	plan := toDispatchPlan(request)
	if plan.Interactive {
		t.Fatal("expected Interactive=false in DispatchPlan, got true")
	}
}
