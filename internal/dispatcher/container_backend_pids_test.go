package dispatcher

import (
	"testing"

	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// TestContainerBackend_Launch_PidsLimitFitsParallelBuilds pins a pids limit
// high enough for a parallel build (e.g. dotnet build on 16 cores) to run next
// to the agent, while still bounding a fork bomb.
func TestContainerBackend_Launch_PidsLimitFitsParallelBuilds(t *testing.T) {
	api := &fakeDockerAPI{}
	be := NewContainerBackend(api, ContainerBackendOptions{})

	mustLaunch(t, be, sandbox.Spec{ID: "job-1", Argv: []string{"true"}}, backend.LaunchOptions{JobID: "job-1"})

	got := api.createCalls[0].HostConfig.Resources.PidsLimit
	if got == nil {
		t.Fatal("HostConfig.Resources.PidsLimit is nil, want a bounded limit")
	}
	if *got != 4096 {
		t.Errorf("HostConfig.Resources.PidsLimit = %d, want 4096", *got)
	}
}
