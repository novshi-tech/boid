package orchestrator_test

import "testing"

func TestCIFailIntentional(t *testing.T) {
	t.Fatal("intentional failure to trigger rework cycle")
}
