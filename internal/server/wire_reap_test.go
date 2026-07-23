package server

import (
	"context"
	"errors"
	"testing"

	"github.com/novshi-tech/boid/internal/dispatcher"
	"github.com/novshi-tech/boid/internal/orchestrator"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/sandbox/backend"
)

// This file pins reapOrphansBeforeReopen — the startup reap-before-reopen
// wiring docs/plans/phase6-container-backend.md §PR7 / §決定6 adds to
// buildRuntime's startup sequence: `MarkStale*` → ReapOrphans →
// daemon_shutdown auto-reopen, with reap-failed jobs' tasks excluded from
// the reopen sweep ("二重実行なし" — a task must not be reopened while its
// previous job's sandbox resource is still known-unreconciled).

// fakeReapBackend is a minimal backend.SandboxBackend whose ReapOrphans
// returns a test-configured ReapReport/error; Launch/Adopt are never
// exercised by these tests.
type fakeReapBackend struct {
	report backend.ReapReport
	err    error
}

var _ backend.SandboxBackend = (*fakeReapBackend)(nil)

func (b *fakeReapBackend) Launch(context.Context, sandbox.Spec, backend.LaunchOptions) (backend.SandboxSession, error) {
	return nil, nil
}
func (b *fakeReapBackend) Adopt(context.Context, string) (backend.SandboxSession, bool) {
	return nil, false
}
func (b *fakeReapBackend) ReapOrphans(context.Context) (backend.ReapReport, error) {
	return b.report, b.err
}

// TestReapOrphansBeforeReopen_SkipsTasksWithFailedJobs pins the plan's own
// worked example almost verbatim ("mock backend で ReapReport{FailedJobIDs:
// []{"job-A"}} → auto-reopen が job-A を skip することを pin"): a task whose
// job failed to reap must be in the returned skip set; a sibling task
// whose job reaped fine (or wasn't mentioned at all) must not be.
func TestReapOrphansBeforeReopen_SkipsTasksWithFailedJobs(t *testing.T) {
	d := openTestDB(t)

	if err := orchestrator.CreateProject(d.Conn, &orchestrator.Project{ID: "proj-1", WorkDir: "/tmp"}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	taskA := &orchestrator.Task{ProjectID: "proj-1", Title: "A", Behavior: "dev", Status: orchestrator.TaskStatusAborted}
	if err := orchestrator.CreateTask(d.Conn, taskA); err != nil {
		t.Fatalf("create task A: %v", err)
	}
	taskB := &orchestrator.Task{ProjectID: "proj-1", Title: "B", Behavior: "dev", Status: orchestrator.TaskStatusAborted}
	if err := orchestrator.CreateTask(d.Conn, taskB); err != nil {
		t.Fatalf("create task B: %v", err)
	}

	jobA := &dispatcher.Job{TaskID: taskA.ID, ProjectID: "proj-1", HandlerID: "h", Status: dispatcher.JobStatusFailed}
	if err := dispatcher.CreateJob(d.Conn, jobA); err != nil {
		t.Fatalf("create job A: %v", err)
	}
	jobB := &dispatcher.Job{TaskID: taskB.ID, ProjectID: "proj-1", HandlerID: "h", Status: dispatcher.JobStatusCompleted}
	if err := dispatcher.CreateJob(d.Conn, jobB); err != nil {
		t.Fatalf("create job B: %v", err)
	}

	be := &fakeReapBackend{report: backend.ReapReport{
		ReapedJobIDs: []string{jobB.ID},
		FailedJobIDs: []string{jobA.ID},
	}}
	runner := &dispatcher.Runner{Backend: be}

	skip, blockAll := reapOrphansBeforeReopen(context.Background(), runner, d.Conn)
	if blockAll {
		t.Fatalf("blockAll = true, want false (no GlobalError set)")
	}

	if !skip[taskA.ID] {
		t.Errorf("skip[%s] = false, want true (job %s is in FailedJobIDs)", taskA.ID, jobA.ID)
	}
	if skip[taskB.ID] {
		t.Errorf("skip[%s] = true, want false (job %s reaped successfully)", taskB.ID, jobB.ID)
	}
}

// TestReapOrphansBeforeReopen_NoFailuresReturnsEmptySkipSet pins the
// steady-state / userns-backend path: a ReapReport with no FailedJobIDs
// (including the userns backend's permanent zero-value stub) must never
// block any task's auto-reopen.
func TestReapOrphansBeforeReopen_NoFailuresReturnsEmptySkipSet(t *testing.T) {
	d := openTestDB(t)
	be := &fakeReapBackend{}
	runner := &dispatcher.Runner{Backend: be}

	skip, blockAll := reapOrphansBeforeReopen(context.Background(), runner, d.Conn)
	if len(skip) != 0 {
		t.Errorf("skip = %v, want empty", skip)
	}
	if blockAll {
		t.Error("blockAll = true, want false (zero-value ReapReport, no GlobalError)")
	}
}

// TestReapOrphansBeforeReopen_UnresolvableJobIDIsSkippedNotFatal pins the
// defensive path: a FailedJobIDs entry that no longer has a DB row (or was
// never linked to a task) must not panic and must simply contribute
// nothing to the skip set — there is no task to protect from a double
// reopen in that case.
func TestReapOrphansBeforeReopen_UnresolvableJobIDIsSkippedNotFatal(t *testing.T) {
	d := openTestDB(t)
	be := &fakeReapBackend{report: backend.ReapReport{FailedJobIDs: []string{"job-does-not-exist"}}}
	runner := &dispatcher.Runner{Backend: be}

	skip, blockAll := reapOrphansBeforeReopen(context.Background(), runner, d.Conn)
	if len(skip) != 0 {
		t.Errorf("skip = %v, want empty (unresolvable job id contributes nothing)", skip)
	}
	if blockAll {
		t.Error("blockAll = true, want false (no GlobalError set)")
	}
}

// TestReapOrphansBeforeReopen_GlobalErrorBlocksAllReopen pins [Blocker 4,
// PR7 codex review]: a non-nil ReapReport.GlobalError (e.g. the docker API
// was entirely unreachable at daemon boot) must set blockAllReopen=true —
// the caller must then withhold auto-reopen for EVERY daemon_shutdown
// -aborted task this boot, not just whatever FailedJobIDs happens to
// enumerate (a total listing failure never got far enough to enumerate
// anything at all, so silently falling back to "skip nothing" would
// auto-reopen every one of them against a possibly-still-alive job
// container — the exact double-execution race §決定6 exists to prevent).
func TestReapOrphansBeforeReopen_GlobalErrorBlocksAllReopen(t *testing.T) {
	d := openTestDB(t)
	globalErr := errors.New("docker unavailable")
	be := &fakeReapBackend{report: backend.ReapReport{GlobalError: globalErr}}
	runner := &dispatcher.Runner{Backend: be}

	skip, blockAll := reapOrphansBeforeReopen(context.Background(), runner, d.Conn)
	if !blockAll {
		t.Fatal("blockAll = false, want true (ReapReport.GlobalError set)")
	}
	if len(skip) != 0 {
		t.Errorf("skip = %v, want empty (GlobalError carries no per-job FailedJobIDs)", skip)
	}
}

// TestReapOrphansBeforeReopen_ReturnedErrorBlocksAllReopen is the sibling of
// the GlobalError test above for ReapOrphans' own (report, err) return —
// the container backend's real implementation sets both together
// (ReapOrphans' own doc comment), but this pins the return-error path
// independently in case a future backend ever returns one without the
// other.
func TestReapOrphansBeforeReopen_ReturnedErrorBlocksAllReopen(t *testing.T) {
	d := openTestDB(t)
	be := &fakeReapBackend{err: errors.New("list orphan containers: connection refused")}
	runner := &dispatcher.Runner{Backend: be}

	_, blockAll := reapOrphansBeforeReopen(context.Background(), runner, d.Conn)
	if !blockAll {
		t.Fatal("blockAll = false, want true (ReapOrphans returned a non-nil error)")
	}
}
