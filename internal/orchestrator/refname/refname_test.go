package refname

import (
	"math/rand/v2"
	"strings"
	"testing"
)

func TestGenerate_Format(t *testing.T) {
	rng := rand.New(rand.NewPCG(42, 0))
	for i := 0; i < 200; i++ {
		name := Generate(rng)
		parts := strings.SplitN(name, "_", 2)
		if len(parts) != 2 {
			t.Fatalf("Generate() = %q, want adjective_noun format (underscore-separated)", name)
		}
		if parts[0] == "" {
			t.Errorf("Generate() = %q: adjective part is empty", name)
		}
		if parts[1] == "" {
			t.Errorf("Generate() = %q: noun part is empty", name)
		}
	}
}

func TestDictionaries_NotEmpty(t *testing.T) {
	if len(adjectives) == 0 {
		t.Error("adjectives dictionary is empty")
	}
	if len(nouns) == 0 {
		t.Error("nouns dictionary is empty")
	}
}

func TestDictionaries_MinSize(t *testing.T) {
	const minSize = 100
	if len(adjectives) < minSize {
		t.Errorf("adjectives dictionary has %d entries, want at least %d", len(adjectives), minSize)
	}
	if len(nouns) < minSize {
		t.Errorf("nouns dictionary has %d entries, want at least %d", len(nouns), minSize)
	}
}
