package sandbox

import (
	"encoding/json"
	"strings"
)

type ExecRequest struct {
	Command   string       `json:"command"`
	Args      []string     `json:"args"`
	Cwd       string       `json:"cwd,omitempty"`
	Stdin     []byte       `json:"stdin,omitempty"`
	Token     string       `json:"token"`
	Boid      *BoidRequest `json:"boid,omitempty"`
	Git       *GitRequest  `json:"git,omitempty"`
	Streaming bool         `json:"streaming,omitempty"`
}

type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// StreamChunk is one unit in the streaming host-command protocol (Streaming=true).
//
// Broker → Shim: type "stdout"/"stderr" carry Data; type "exit" carries ExitCode.
// Shim → Broker: type "kill" requests SIGTERM on the host process group.
const (
	StreamTypeStdout = "stdout"
	StreamTypeStderr = "stderr"
	StreamTypeExit   = "exit"
	StreamTypeKill   = "kill"
)

type StreamChunk struct {
	Type     string `json:"type"`
	Data     string `json:"data,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
}

type BoidOp string

const (
	BoidOpJobDone    BoidOp = "job_done"
	BoidOpTaskCreate BoidOp = "task_create"
	BoidOpTaskGet    BoidOp = "task_get"
	BoidOpTaskUpdate BoidOp = "task_update"
	BoidOpTaskImport BoidOp = "task_import"
	BoidOpTaskReopen BoidOp = "task.reopen"
	BoidOpTaskList   BoidOp = "task_list"
	BoidOpTaskNotify BoidOp = "task_notify"
	BoidOpTaskAnswer BoidOp = "task_answer"
)

// BehaviorSpec is the inline behavior specification carried in BoidRequest.
// It mirrors orchestrator.BehaviorSpec but is defined here to keep the
// sandbox package free of orchestrator dependencies.
type BehaviorSpec struct {
	Name           string   `yaml:"name" json:"name"`
	Traits         []string `yaml:"traits,omitempty" json:"traits,omitempty"`
	Readonly       bool     `yaml:"readonly,omitempty" json:"readonly,omitempty"`
	Worktree       bool     `yaml:"worktree,omitempty" json:"worktree,omitempty"`
	BranchPrefix   string   `yaml:"branch_prefix,omitempty" json:"branch_prefix,omitempty"`
	BaseBranch     string   `yaml:"base_branch,omitempty" json:"base_branch,omitempty"`
	DefaultPayload []byte   `yaml:"-" json:"default_payload,omitempty"`
}

type BoidRequest struct {
	Op               BoidOp        `json:"op"`
	JobID            string        `json:"job_id,omitempty"`
	TaskID           string        `json:"task_id,omitempty"`
	TaskField        string        `json:"task_field,omitempty"`
	ProjectID        string        `json:"project_id,omitempty"`
	Title            string        `json:"title,omitempty"`
	Behavior         string        `json:"behavior,omitempty"`
	BehaviorSpec     *BehaviorSpec `json:"behavior_spec,omitempty"`
	BaseBranch       string        `json:"base_branch,omitempty"`
	Description      string        `json:"description,omitempty"`
	ExitCode         int           `json:"exit_code,omitempty"`
	Output           string        `json:"output,omitempty"`
	Payload          []byte        `json:"payload,omitempty"`
	Ref              string        `json:"ref,omitempty"`
	ParentID         string        `json:"parent_id,omitempty"`
	DependsOn        []string      `json:"depends_on,omitempty"`
	DependsOnPayload string        `json:"depends_on_payload,omitempty"`
	AutoStart        bool          `json:"auto_start,omitempty"`

	// task import fields
	ImportTasks              []json.RawMessage `json:"import_tasks,omitempty"`
	ImportProjectOverride    string            `json:"import_project_override,omitempty"`
	ImportDatasourceOverride string            `json:"import_datasource_override,omitempty"`

	// task list fields
	WorkspaceID string `json:"workspace_id,omitempty"`
	Status      string `json:"status,omitempty"`
	Limit       int    `json:"limit,omitempty"`

	// task notify fields
	Message    string `json:"message,omitempty"`
	Ask        string `json:"ask,omitempty"`
	QuestionID string `json:"question_id,omitempty"`
	SessionID  string `json:"session_id,omitempty"`

	// task answer fields
	Answer string `json:"answer,omitempty"`
}

type TokenContext struct {
	JobID             string
	TaskID            string
	ProjectID         string
	WorkspaceID       string
	AllowedProjectIDs []string
	Role              string
	// ProjectDir is the project's host-side work directory. Independent of
	// spec.Visibility.ProjectDir (which drives sandbox mount layout and is
	// intentionally empty for gate jobs): host-side operations the broker
	// performs on behalf of the sandbox (git binding, host-command cwd) have
	// their own notion of "which project are we operating on" that doesn't
	// care whether the sandbox itself can see the tree.
	ProjectDir  string
	WorktreeDir string
}

func (c TokenContext) AllowsProject(projectID string) bool {
	if projectID == "" {
		projectID = c.ProjectID
	}
	if projectID == "" {
		return false
	}
	if len(c.AllowedProjectIDs) == 0 {
		return projectID == c.ProjectID
	}
	for _, allowed := range c.AllowedProjectIDs {
		if allowed == projectID {
			return true
		}
	}
	return false
}

// ProjectResolver maps a project reference (UUID, exact name, or partial name)
// to a concrete project UUID. The broker calls it just before the UUID-based
// AllowsProject authorization check so that sandbox-side callers (e.g. plan
// agents) can use project names while the broker continues to enforce
// workspace isolation in UUID space. When nil, the broker passes refs through
// verbatim (tests and UUID-only callers).
type ProjectResolver func(ref string) (uuid string, err error)

type GitOp string

const (
	GitOpFetch      GitOp = "fetch"
	GitOpPush       GitOp = "push"
	GitOpPushDelete GitOp = "push_delete"
)

// BuiltinPolicy defines which operations are permitted for a named builtin command.
// It is stamped at token registration time by the planner and checked at dispatch time
// by the broker, keeping all role-based authorization logic outside the broker itself.
type BuiltinPolicy struct {
	AllowedOps map[string]struct{}
	// AllowedCwdRoots lists additional cwd roots permitted for this builtin
	// beyond the per-token entry root (project/worktree dir). Used so that
	// e.g. gate jobs can target /tmp or the host project dir without the
	// broker needing to know the role itself.
	AllowedCwdRoots []string
}

// Allows reports whether op is in the allowed set.
func (p BuiltinPolicy) Allows(op string) bool {
	_, ok := p.AllowedOps[op]
	return ok
}

// AllowsCwd reports whether cwd is within any of the policy's additional cwd roots.
func (p BuiltinPolicy) AllowsCwd(cwd string) bool {
	for _, root := range p.AllowedCwdRoots {
		if root == "" {
			continue
		}
		if cwd == root {
			return true
		}
		if strings.HasPrefix(cwd, root+"/") {
			return true
		}
	}
	return false
}

type GitRequest struct {
	Op             GitOp    `json:"op"`
	Remote         string   `json:"remote,omitempty"`
	Refspecs       []string `json:"refspecs,omitempty"`
	DryRun         bool     `json:"dry_run,omitempty"`
	Verbose        bool     `json:"verbose,omitempty"`
	Quiet          bool     `json:"quiet,omitempty"`
	Prune          bool     `json:"prune,omitempty"`
	Tags           bool     `json:"tags,omitempty"`
	Force          bool     `json:"force,omitempty"`
	Porcelain      bool     `json:"porcelain,omitempty"`
	ForceWithLease bool     `json:"force_with_lease,omitempty"`
	Delete         bool     `json:"delete,omitempty"`
}
