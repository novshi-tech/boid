package server_test

import (
	"testing"

	"github.com/novshi-tech/boid/internal/api"
)

// TestServer_ExecDispatch_ExitCodeRoundTrips is a server-level regression
// guard for PR #735 (git gateway cutover's exec-via-Dispatch()): it drives
// the exact HTTP round trip `boid exec` depends on without a real sandbox —
//
//	POST /api/projects/{id}/exec   (sessionDispatcherAdapter.StartExec →
//	                                 dispatcher.BuildExecJobSpec → Runner.Dispatch())
//	POST /api/jobs/{id}/done       (mirrors the sandboxed runner-inner-child's
//	                                 own completion call → TaskWorkflowService.CompleteJob)
//	GET  /api/jobs/{id}            (mirrors cmd/exec.go's fetchExecExitCode poll)
//
// This exists because the e2e scenario asserting the same behavior
// (e2e/scenarios/exec-smoke) is currently not a reliable gate: e2e/run.sh's
// `(source scenario.sh) > >(tee ...) 2>&1` swallows a failing scenario's exit
// code (see PR #735's "known issues" note / Opus review finding #2), so a
// regression here would not actually fail CI today. noopRuntime (defined in
// server_phase6_test.go) never auto-completes a dispatched job, so nothing
// but this test's explicit POST .../done call can resolve it.
func TestServer_ExecDispatch_ExitCodeRoundTrips(t *testing.T) {
	t.Run("nonzero exit", func(t *testing.T) {
		ts := newSmokeServer(t)
		projectDir := writeSmokeProject(t)

		var project struct {
			ID string `json:"id"`
		}
		if err := ts.Client.Do("POST", "/api/projects", map[string]any{
			"work_dir": projectDir,
		}, &project); err != nil {
			t.Fatalf("create project: %v", err)
		}

		var exec api.StartExecResult
		if err := ts.Client.Do("POST", "/api/projects/"+project.ID+"/exec", api.StartExecRequest{
			Argv: []string{"bash", "-c", "exit 42"},
		}, &exec); err != nil {
			t.Fatalf("start exec: %v", err)
		}
		if exec.JobID == "" {
			t.Fatal("expected non-empty job id from StartExec")
		}

		// Best-effort: a task-less job's failure path in CompleteJob additionally
		// tries to apply a job_failed task transition, which has no task to find
		// here and can surface as an HTTP error — but job.ExitCode is persisted
		// via JobStore.UpdateJob before that lookup runs, so the DB write this
		// test cares about has already landed regardless of the response.
		_ = ts.Client.Do("POST", "/api/jobs/"+exec.JobID+"/done", map[string]any{
			"exit_code": 42,
			"output":    "boom",
		}, nil)

		var job api.Job
		if err := ts.Client.Do("GET", "/api/jobs/"+exec.JobID, nil, &job); err != nil {
			t.Fatalf("get job: %v", err)
		}
		if job.ExitCode != 42 {
			t.Fatalf("job.ExitCode = %d, want 42 (fetchExecExitCode's read-back contract)", job.ExitCode)
		}
		if job.Status != api.JobStatusFailed {
			t.Fatalf("job.Status = %q, want %q", job.Status, api.JobStatusFailed)
		}
	})

	t.Run("zero exit", func(t *testing.T) {
		ts := newSmokeServer(t)
		projectDir := writeSmokeProject(t)

		var project struct {
			ID string `json:"id"`
		}
		if err := ts.Client.Do("POST", "/api/projects", map[string]any{
			"work_dir": projectDir,
		}, &project); err != nil {
			t.Fatalf("create project: %v", err)
		}

		var exec api.StartExecResult
		if err := ts.Client.Do("POST", "/api/projects/"+project.ID+"/exec", api.StartExecRequest{
			Argv: []string{"true"},
		}, &exec); err != nil {
			t.Fatalf("start exec: %v", err)
		}

		if err := ts.Client.Do("POST", "/api/jobs/"+exec.JobID+"/done", map[string]any{
			"exit_code": 0,
		}, nil); err != nil {
			t.Fatalf("complete job: %v", err)
		}

		var job api.Job
		if err := ts.Client.Do("GET", "/api/jobs/"+exec.JobID, nil, &job); err != nil {
			t.Fatalf("get job: %v", err)
		}
		if job.ExitCode != 0 {
			t.Fatalf("job.ExitCode = %d, want 0", job.ExitCode)
		}
		if job.Status != api.JobStatusCompleted {
			t.Fatalf("job.Status = %q, want %q", job.Status, api.JobStatusCompleted)
		}
	})
}
