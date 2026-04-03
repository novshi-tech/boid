package sandbox

type ExecRequest struct {
	Command string       `json:"command"`
	Args    []string     `json:"args"`
	Cwd     string       `json:"cwd,omitempty"`
	Stdin   []byte       `json:"stdin,omitempty"`
	Token   string       `json:"token"`
	Boid    *BoidRequest `json:"boid,omitempty"`
	Git     *GitRequest  `json:"git,omitempty"`
}

type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

type BoidOp string

const (
	BoidOpJobDone    BoidOp = "job_done"
	BoidOpTaskCreate BoidOp = "task_create"
	BoidOpTaskGet    BoidOp = "task_get"
)

type BoidRequest struct {
	Op          BoidOp `json:"op"`
	JobID       string `json:"job_id,omitempty"`
	TaskID      string `json:"task_id,omitempty"`
	TaskField   string `json:"task_field,omitempty"`
	ProjectID   string `json:"project_id,omitempty"`
	Title       string `json:"title,omitempty"`
	Behavior    string `json:"behavior,omitempty"`
	Description string `json:"description,omitempty"`
	ExitCode    int    `json:"exit_code,omitempty"`
	Output      string `json:"output,omitempty"`
	Payload     []byte `json:"payload,omitempty"`
}

type TokenContext struct {
	JobID             string
	TaskID            string
	ProjectID         string
	WorkspaceID       string
	AllowedProjectIDs []string
	Role              string
	ProjectDir        string
	WorktreeDir       string
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

type GitOp string

const (
	GitOpFetch GitOp = "fetch"
	GitOpPush  GitOp = "push"
)

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
}
