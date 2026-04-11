package kit_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/kit"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestValidateRequirements_AllPresent(t *testing.T) {
	kits := []orchestrator.KitMeta{
		{
			Meta:     &orchestrator.KitMetaInfo{Name: "go-kit"},
			Requires: &orchestrator.KitRequires{Commands: []string{"sh"}},
		},
	}
	if errs := kit.ValidateRequirements(kits); len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateRequirements_MissingCommand(t *testing.T) {
	kits := []orchestrator.KitMeta{
		{
			Meta:     &orchestrator.KitMetaInfo{Name: "my-kit"},
			Requires: &orchestrator.KitRequires{Commands: []string{"__boid_nonexistent_xyz__"}},
		},
	}
	errs := kit.ValidateRequirements(kits)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if errs[0].KitName != "my-kit" || errs[0].Command != "__boid_nonexistent_xyz__" {
		t.Errorf("unexpected error: %+v", errs[0])
	}
}

func TestValidateRequirements_NoRequires(t *testing.T) {
	kits := []orchestrator.KitMeta{{Meta: &orchestrator.KitMetaInfo{Name: "no-reqs"}}}
	if errs := kit.ValidateRequirements(kits); len(errs) != 0 {
		t.Fatalf("expected no errors for nil Requires, got %v", errs)
	}
}

func TestValidateRequirements_MultipleKits(t *testing.T) {
	kits := []orchestrator.KitMeta{
		{
			Meta:     &orchestrator.KitMetaInfo{Name: "kit-a"},
			Requires: &orchestrator.KitRequires{Commands: []string{"sh", "__boid_miss_a__"}},
		},
		{
			Meta:     &orchestrator.KitMetaInfo{Name: "kit-b"},
			Requires: &orchestrator.KitRequires{Commands: []string{"__boid_miss_b__"}},
		},
	}
	errs := kit.ValidateRequirements(kits)
	if len(errs) != 2 {
		t.Fatalf("expected 2 errors, got %d: %v", len(errs), errs)
	}
}

func TestValidateRequirements_NoMeta(t *testing.T) {
	kits := []orchestrator.KitMeta{
		{Requires: &orchestrator.KitRequires{Commands: []string{"__boid_no_meta__"}}},
	}
	errs := kit.ValidateRequirements(kits)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if errs[0].KitName != "" {
		t.Errorf("expected empty KitName when Meta is nil, got %q", errs[0].KitName)
	}
}

func TestRequirementError_Error(t *testing.T) {
	t.Run("with kit name", func(t *testing.T) {
		e := kit.RequirementError{KitName: "my-kit", Command: "go"}
		got := e.Error()
		if !strings.Contains(got, "my-kit") || !strings.Contains(got, "go") {
			t.Errorf("Error() = %q, want both kit name and command", got)
		}
	})
	t.Run("without kit name", func(t *testing.T) {
		e := kit.RequirementError{Command: "docker"}
		got := e.Error()
		if !strings.Contains(got, "docker") {
			t.Errorf("Error() = %q, want command name", got)
		}
	})
}
