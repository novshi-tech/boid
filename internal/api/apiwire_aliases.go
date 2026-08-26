// This file re-exports every symbol internal/apiwire owns back into
// package api.
//
// The types below are the daemon↔client wire contract. They moved to
// internal/apiwire so the CLI could be built for GOOS=windows/darwin
// (see that package's doc.go); they are aliases rather than a rename so
// that nothing on the SERVER side had to change — handlers, services,
// internal/server's wiring and web/templates all keep spelling them
// api.Job, api.WorkspaceDetail and so on.
//
// Aliases, not new named types: `type Job = apiwire.Job` makes the two
// spellings the same type, so a value crossing between a handler and the
// client package needs no conversion and no second JSON round trip.
//
// NormalizePublicURL is deliberately NOT re-exported here — it is a
// function, aliasing it would need a package-level var, and it has three
// call sites in total. They name apiwire directly.

package api

import "github.com/novshi-tech/boid/internal/apiwire"

// from internal/api/action.go
type ApplyActionRequest = apiwire.ApplyActionRequest

// from internal/api/service.go
type ActionApplication = apiwire.ActionApplication
type TaskDetailView = apiwire.TaskDetailView

// from internal/api/job_model.go
type JobStatus = apiwire.JobStatus

const JobStatusRunning = apiwire.JobStatusRunning
const JobStatusCompleted = apiwire.JobStatusCompleted
const JobStatusFailed = apiwire.JobStatusFailed

type Job = apiwire.Job
type JobWithContext = apiwire.JobWithContext
type JobListFilter = apiwire.JobListFilter

// from internal/api/config.go
type ConfigApplyResult = apiwire.ConfigApplyResult
type ConfigMutateOp = apiwire.ConfigMutateOp

const ConfigMutateSet = apiwire.ConfigMutateSet
const ConfigMutateUnset = apiwire.ConfigMutateUnset

type ConfigMutateRequest = apiwire.ConfigMutateRequest
type ConfigMutateResult = apiwire.ConfigMutateResult

// from internal/api/workspace_homes.go
type WorkspaceHomeSize = apiwire.WorkspaceHomeSize
type WorkspaceRemoveResponse = apiwire.WorkspaceRemoveResponse

// from internal/api/store.go
type ReplayHookResult = apiwire.ReplayHookResult
type WorkspaceDetail = apiwire.WorkspaceDetail
type ImportError = apiwire.ImportError
type ImportResult = apiwire.ImportResult
type StartSessionRequest = apiwire.StartSessionRequest
type StartSessionResult = apiwire.StartSessionResult
type StartExecRequest = apiwire.StartExecRequest
type StartExecResult = apiwire.StartExecResult

// from internal/api/task.go
type NotifyTaskRequest = apiwire.NotifyTaskRequest
type AnswerTaskRequest = apiwire.AnswerTaskRequest
type UpdateTaskRequest = apiwire.UpdateTaskRequest
type CreateTaskRequest = apiwire.CreateTaskRequest
type DuplicateTaskRequest = apiwire.DuplicateTaskRequest
type RerunTaskRequest = apiwire.RerunTaskRequest

// from internal/api/workspace_init_script.go
const WorkspaceInitScriptContentType = apiwire.WorkspaceInitScriptContentType
const WorkspaceInitScriptAbsentRevision = apiwire.WorkspaceInitScriptAbsentRevision
const WorkspaceInitScriptWritten = apiwire.WorkspaceInitScriptWritten
const WorkspaceInitScriptCleared = apiwire.WorkspaceInitScriptCleared
const WorkspaceInitScriptUnchanged = apiwire.WorkspaceInitScriptUnchanged

type WorkspaceInitScriptResult = apiwire.WorkspaceInitScriptResult

// from internal/api/signal_handler.go
type SignalSource = apiwire.SignalSource
type Signal = apiwire.Signal
type ListSignalsResponse = apiwire.ListSignalsResponse
type AckSignalsRequest = apiwire.AckSignalsRequest
type AckSignalsResponse = apiwire.AckSignalsResponse
