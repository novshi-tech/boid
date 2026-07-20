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

	OpFetchGet = "get"
)
