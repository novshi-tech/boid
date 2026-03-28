package db_test

import (
	"strings"
	"testing"

	"github.com/novshi-tech/boid/internal/model"
	"github.com/novshi-tech/boid/testutil"
)

func TestCreateJob(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &model.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := d.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	job := &model.Job{
		TaskID:    task.ID,
		ProjectID: "proj-1",
		HookID:    "hook-1",
	}
	if err := d.CreateJob(job); err != nil {
		t.Fatalf("create job: %v", err)
	}
	if job.ID == "" {
		t.Fatal("expected auto-generated ID")
	}
	if job.Status != model.JobStatusRunning {
		t.Fatalf("expected default status running, got %s", job.Status)
	}
	if job.CreatedAt.IsZero() {
		t.Fatal("expected CreatedAt to be set")
	}
}

func TestGetJob(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &model.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := d.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	job := &model.Job{
		TaskID:    task.ID,
		ProjectID: "proj-1",
		HookID:    "hook-1",
	}
	if err := d.CreateJob(job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	got, err := d.GetJob(job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.ID != job.ID {
		t.Fatalf("expected id %s, got %s", job.ID, got.ID)
	}
	if got.TaskID != task.ID {
		t.Fatalf("expected task_id %s, got %s", task.ID, got.TaskID)
	}
	if got.ProjectID != "proj-1" {
		t.Fatalf("expected project_id proj-1, got %s", got.ProjectID)
	}
	if got.HookID != "hook-1" {
		t.Fatalf("expected hook_id hook-1, got %s", got.HookID)
	}
	if got.Status != model.JobStatusRunning {
		t.Fatalf("expected running, got %s", got.Status)
	}
}

func TestGetJob_NotFound(t *testing.T) {
	d := testutil.NewTestDB(t)
	_, err := d.GetJob("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent job")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found error, got: %v", err)
	}
}

func TestListJobsByTask(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task1 := &model.Task{ProjectID: "proj-1", Title: "Task1", Behavior: "dev"}
	if err := d.CreateTask(task1); err != nil {
		t.Fatalf("create task1: %v", err)
	}
	task2 := &model.Task{ProjectID: "proj-1", Title: "Task2", Behavior: "dev"}
	if err := d.CreateTask(task2); err != nil {
		t.Fatalf("create task2: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := d.CreateJob(&model.Job{TaskID: task1.ID, ProjectID: "proj-1", HookID: "hook-1"}); err != nil {
			t.Fatalf("create job: %v", err)
		}
	}
	if err := d.CreateJob(&model.Job{TaskID: task2.ID, ProjectID: "proj-1", HookID: "hook-1"}); err != nil {
		t.Fatalf("create job: %v", err)
	}

	jobs, err := d.ListJobsByTask(task1.ID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs for task1, got %d", len(jobs))
	}

	jobs, err = d.ListJobsByTask(task2.ID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job for task2, got %d", len(jobs))
	}
}

func TestListJobsByTask_Empty(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &model.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := d.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	jobs, err := d.ListJobsByTask(task.ID)
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("expected 0 jobs, got %d", len(jobs))
	}
}

func TestUpdateJob(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &model.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := d.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	job := &model.Job{
		TaskID:    task.ID,
		ProjectID: "proj-1",
		HookID:    "hook-1",
	}
	if err := d.CreateJob(job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	job.Status = model.JobStatusCompleted
	job.ExitCode = 0
	job.Output = "success"
	if err := d.UpdateJob(job); err != nil {
		t.Fatalf("update job: %v", err)
	}

	got, err := d.GetJob(job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != model.JobStatusCompleted {
		t.Fatalf("expected completed, got %s", got.Status)
	}
	if got.ExitCode != 0 {
		t.Fatalf("expected exit_code 0, got %d", got.ExitCode)
	}
	if got.Output != "success" {
		t.Fatalf("expected output 'success', got %s", got.Output)
	}
}

func TestUpdateJob_Failed(t *testing.T) {
	d := testutil.NewTestDB(t)
	createTestProject(t, d)

	task := &model.Task{ProjectID: "proj-1", Title: "Task", Behavior: "dev"}
	if err := d.CreateTask(task); err != nil {
		t.Fatalf("create task: %v", err)
	}

	job := &model.Job{
		TaskID:    task.ID,
		ProjectID: "proj-1",
		HookID:    "hook-1",
	}
	if err := d.CreateJob(job); err != nil {
		t.Fatalf("create job: %v", err)
	}

	job.Status = model.JobStatusFailed
	job.ExitCode = 1
	job.Output = "error occurred"
	if err := d.UpdateJob(job); err != nil {
		t.Fatalf("update job: %v", err)
	}

	got, err := d.GetJob(job.ID)
	if err != nil {
		t.Fatalf("get job: %v", err)
	}
	if got.Status != model.JobStatusFailed {
		t.Fatalf("expected failed, got %s", got.Status)
	}
	if got.ExitCode != 1 {
		t.Fatalf("expected exit_code 1, got %d", got.ExitCode)
	}
}
