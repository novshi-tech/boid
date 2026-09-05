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
	OpBoidTaskWait   = "task_wait"

	// Task-context RPCs.
	OpBoidTaskCurrent      = "task_current"
	OpBoidTaskInstructions = "task_instructions"
	OpBoidTaskEnv          = "task_env"
	OpBoidTaskPayload      = "task_payload"

	// Attachments RPCs.
	OpBoidTaskAttachmentsList = "task_attachments_list"
	OpBoidTaskAttachmentsGet  = "task_attachments_get"

	// job_done payload_patch direct-pass RPC.
	OpBoidTaskUpdatePayloadPatch = "task_update_payload_patch"

	// OpBoidProjectBehaviors backs `boid project behaviors` from inside the sandbox.
	OpBoidProjectBehaviors = "project_behaviors"
	// OpBoidProjectList backs `boid project list` from inside the sandbox —
	// discovery companion to OpBoidProjectBehaviors, scoped to the caller's
	// own workspace (see sandbox.BoidOpProjectList's doc comment).
	OpBoidProjectList = "project_list"

	// OpBoidCardGet / OpBoidCardList back `boid card get` / `boid card list`
	// from inside the sandbox: the read half of "daemon is the sole source
	// of state truth".
	OpBoidCardGet  = "card_get"
	OpBoidCardList = "card_list"

	// OpBoidTaskIdentityLink / OpBoidTaskIdentityUnlink / OpBoidTaskIdentityResolve
	// back `boid task identity link/unlink/resolve` from inside the
	// sandbox: the identity index's only external surface. No HTTP route
	// exists for these (the sandbox shim is the sole caller today).
	OpBoidTaskIdentityLink    = "task_identity_link"
	OpBoidTaskIdentityUnlink  = "task_identity_unlink"
	OpBoidTaskIdentityResolve = "task_identity_resolve"

	// OpBoidTaskResolveOrCapture backs `boid task resolve-or-capture` from
	// inside the sandbox: the destination-resolution op — resolve Identity
	// to an existing task, or atomically create a new `captured` triage
	// task and link it when unresolved. Same "no HTTP route" scoping as the
	// identity ops above.
	OpBoidTaskResolveOrCapture = "task_resolve_or_capture"

	// OpBoidActionList backs `boid action list` from inside the sandbox:
	// the workspace-scoped since-cursor read over actions — the missing
	// read half of OpBoidActionSend.
	OpBoidActionList = "action_list"

	// OpBoidSignalList / OpBoidSignalAck back `boid signal list` / `boid
	// signal ack` from inside the sandbox — part of the general boidPolicy
	// (see boidPolicy's own doc comment in policy.go).
	OpBoidSignalList = "signal_list"
	OpBoidSignalAck  = "signal_ack"
	// OpBoidSignalClaim backs `boid signal claim <id>...` — the explicit
	// "these are the rows I handed to a judgment" half of the read/claim
	// split. Granted by the same general boidPolicy as signal_list/
	// signal_ack: a job that may read its workspace's inbox and ack a row
	// may also say which rows it took.
	OpBoidSignalClaim = "signal_claim"

	// OpBoidSignalIngest / OpBoidSignalCursorGet mirror
	// sandbox.BoidOpSignalIngest / BoidOpSignalCursorGet — declared here for
	// mirror-table / drift-check completeness only. Deliberately NOT added
	// to boidPolicy's AllowedOps (policy.go): these are granted only via a
	// connector-scoped reduced policy.
	OpBoidSignalIngest    = "signal_ingest"
	OpBoidSignalCursorGet = "signal_cursor_get"

	OpFetchGet = "get"
)
