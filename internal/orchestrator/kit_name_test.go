package orchestrator_test

import (
	"testing"

	kit "github.com/novshi-tech/boid/internal/orchestrator"
)

func TestValidKitName(t *testing.T) {
	valid := []string{"go", "go-tools", "node-lts", "a", "123", "go123"}
	for _, s := range valid {
		if err := kit.ValidKitName(s); err != nil {
			t.Errorf("ValidKitName(%q) = %v, want nil", s, err)
		}
	}

	invalid := []string{
		"",
		"Go",
		"go_tools",
		"go/tools",
		"go tools",
		"../etc",
		"github.com/user/repo",
	}
	for _, s := range invalid {
		if err := kit.ValidKitName(s); err == nil {
			t.Errorf("ValidKitName(%q) = nil, want error", s)
		}
	}

	// Too long (65 chars)
	long := ""
	for i := 0; i < 65; i++ {
		long += "a"
	}
	if err := kit.ValidKitName(long); err == nil {
		t.Error("expected error for name exceeding 64 chars")
	}
}
