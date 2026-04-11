package kit_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/kit"
	"github.com/novshi-tech/boid/internal/orchestrator"
)

func TestValidateRequirements_AllPresent(t *testing.T) {
	kits := []orchestrator.KitMeta{
		{
			Meta: &orchestrator.KitMetaInfo{Name: "go-kit"},
			Requires: &orchestrator.KitRequires{
				Commands: []string{"sh"}, // always present
			},
		},
	}
	errs := kit.ValidateRequirements(kits)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateRequirements_MissingCommand(t *testing.T) {
	kits := []orchestrator.KitMeta{
		{
			Meta: &orchestrator.KitMetaInfo{Name: "test-kit"},
			Requires: &orchestrator.KitRequires{
				Commands: []string{"__boid_nonexistent_cmd_xyz__"},
			},
		},
	}
	errs := kit.ValidateRequirements(kits)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d: %v", len(errs), errs)
	}
	if errs[0].KitName != "test-kit" {
		t.Errorf("expected KitName %q, got %q", "test-kit", errs[0].KitName)
	}
	if errs[0].Command != "__boid_nonexistent_cmd_xyz__" {
		t.Errorf("unexpected Command: %q", errs[0].Command)
	}
}

func TestValidateRequirements_NoRequires(t *testing.T) {
	kits := []orchestrator.KitMeta{
		{
			Meta: &orchestrator.KitMetaInfo{Name: "bare-kit"},
		},
	}
	errs := kit.ValidateRequirements(kits)
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got %v", errs)
	}
}

func TestValidateRequirements_NoMeta(t *testing.T) {
	// Kit without Meta field should still validate requires.commands.
	kits := []orchestrator.KitMeta{
		{
			Requires: &orchestrator.KitRequires{
				Commands: []string{"__boid_nonexistent_cmd_xyz__"},
			},
		},
	}
	errs := kit.ValidateRequirements(kits)
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(errs))
	}
	if errs[0].KitName != "" {
		t.Errorf("expected empty KitName, got %q", errs[0].KitName)
	}
}

func TestValidateRequirements_MultipleKits(t *testing.T) {
	kits := []orchestrator.KitMeta{
		{
			Meta:     &orchestrator.KitMetaInfo{Name: "kit-a"},
			Requires: &orchestrator.KitRequires{Commands: []string{"sh"}},
		},
		{
			Meta:     &orchestrator.KitMetaInfo{Name: "kit-b"},
			Requires: &orchestrator.KitRequires{Commands: []string{"__boid_missing_1__", "__boid_missing_2__"}},
		},
		{
			// No requires at all.
			Meta: &orchestrator.KitMetaInfo{Name: "kit-c"},
		},
	}
	errs := kit.ValidateRequirements(kits)
	if len(errs) != 2 {
		t.Fatalf("expected 2 errors, got %d: %v", len(errs), errs)
	}
	for _, e := range errs {
		if e.KitName != "kit-b" {
			t.Errorf("expected KitName %q, got %q", "kit-b", e.KitName)
		}
	}
}

func TestRequirementError_Error(t *testing.T) {
	t.Run("with kit name", func(t *testing.T) {
		e := kit.RequirementError{KitName: "my-kit", Command: "docker"}
		got := e.Error()
		want := `kit "my-kit": command "docker" not found in PATH`
		if got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})

	t.Run("without kit name", func(t *testing.T) {
		e := kit.RequirementError{Command: "docker"}
		got := e.Error()
		want := `command "docker" not found in PATH`
		if got != want {
			t.Errorf("Error() = %q, want %q", got, want)
		}
	})
}
