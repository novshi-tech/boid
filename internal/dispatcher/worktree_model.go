package dispatcher

import "time"

type Worktree struct {
	ID         string     `json:"id"`
	TaskID     string     `json:"task_id"`
	ProjectID  string     `json:"project_id"`
	Path       string     `json:"path"`
	Branch     string     `json:"branch"`
	BaseBranch string     `json:"base_branch"`
	CreatedAt  time.Time  `json:"created_at"`
	CleanedAt  *time.Time `json:"cleaned_at,omitempty"`
}
