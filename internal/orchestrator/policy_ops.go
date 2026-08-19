package orchestrator

// Mirror of sandbox layer's op constants. orchestrator cannot import sandbox
// (that would reverse the layer direction), so these are kept as string
// literals that must stay in lock-step with internal/sandbox/protocol.go.
// Drift is detected by internal/dispatcher/policy_translate_test.go — the
// only layer allowed to see both sides.
const (
	OpBoidJobDone    = "job_done"
	OpBoidJobList    = "job_list"
	OpBoidJobShow    = "job_show"
	OpBoidJobLog     = "job_log"
	OpBoidActionSend = "action_send"
	OpBoidAgentStop  = "agent_stop"
	OpBoidTaskCreate = "task_create"
	OpBoidTaskGet    = "task_get"
	OpBoidTaskUpdate = "task_update"
	OpBoidTaskImport = "task_import"
	OpBoidTaskReopen = "task.reopen"
	OpBoidTaskList   = "task_list"
	OpBoidTaskNotify = "task_notify"
	OpBoidTaskAnswer = "task_answer"
	OpBoidTaskAsk    = "task_ask"
	OpBoidTaskDelete = "task_delete"

	// Phase 5b PR1 task-context RPCs (docs/plans/phase5-shim-and-task-context.md).
	OpBoidTaskCurrent      = "task_current"
	OpBoidTaskInstructions = "task_instructions"
	OpBoidTaskEnv          = "task_env"
	OpBoidTaskPayload      = "task_payload"

	// Phase 5b PR2 attachments RPCs (docs/plans/phase5-shim-and-task-context.md).
	OpBoidTaskAttachmentsList = "task_attachments_list"
	OpBoidTaskAttachmentsGet  = "task_attachments_get"

	// Phase 5b PR7 job_done payload_patch direct-pass RPC
	// (docs/plans/phase5-shim-and-task-context.md).
	OpBoidTaskUpdatePayloadPatch = "task_update_payload_patch"

	// OpBoidProjectBehaviors backs `boid project behaviors` from inside the
	// sandbox (docs/plans/workspace-default-project.md follow-up).
	OpBoidProjectBehaviors = "project_behaviors"
	// OpBoidProjectList backs `boid project list` from inside the sandbox —
	// discovery companion to OpBoidProjectBehaviors, scoped to the caller's
	// own workspace (see sandbox.BoidOpProjectList's doc comment).
	OpBoidProjectList = "project_list"

	// OpBoidTaskWake backs `boid task wake` from inside the sandbox
	// (docs/plans/cross-project-issue-triage.md Phase 1 PR-4 論点10).
	OpBoidTaskWake = "task_wake"

	// OpBoidTaskTriageGet / OpBoidTaskTriageList back `boid task triage`
	// from inside the sandbox (docs/plans/cross-project-issue-triage.md Phase
	// 1 PR-5a 決定14): the read half of "daemon が state の唯一の正".
	OpBoidTaskTriageGet  = "task_triage_get"
	OpBoidTaskTriageList = "task_triage_list"

	// OpBoidTaskIdentityLink / OpBoidTaskIdentityUnlink / OpBoidTaskIdentityResolve
	// back `boid task identity link/unlink/resolve` from inside the sandbox
	// (docs/plans/ingestion-identity.md PR-1, B-1): the identity index's
	// only external surface. No HTTP route exists for these (the sandbox
	// shim is the sole caller today).
	OpBoidTaskIdentityLink    = "task_identity_link"
	OpBoidTaskIdentityUnlink  = "task_identity_unlink"
	OpBoidTaskIdentityResolve = "task_identity_resolve"

	OpFetchGet = "get"
)
