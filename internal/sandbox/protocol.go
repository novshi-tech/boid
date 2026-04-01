package sandbox

type ExecRequest struct {
	Command string      `json:"command"`
	Args    []string    `json:"args"`
	Cwd     string      `json:"cwd,omitempty"`
	Stdin   []byte      `json:"stdin,omitempty"`
	Token   string      `json:"token"`
	Git     *GitRequest `json:"git,omitempty"`
}

type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
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
