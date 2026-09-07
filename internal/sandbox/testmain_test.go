package sandbox_test

import (
	"os"
	"testing"

	"github.com/novshi-tech/boid/testutil/homeenv"
)

// TestMain sheds the ambient broker wiring these tests would otherwise reach
// instead of the fake broker each one starts.
func TestMain(m *testing.M) {
	os.Exit(homeenv.Run(m))
}

// TestSandboxSuite_IsIsolatedFromRealUserHome is the standing guard for the
// property TestMain provides; losing the TestMain is otherwise silent.
func TestSandboxSuite_IsIsolatedFromRealUserHome(t *testing.T) {
	homeenv.AssertIsolated(t)
}
