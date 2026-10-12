package apiwire

// TaskIdentityRequest selects a repository scope or a repository-free workspace scope.
type TaskIdentityRequest struct {
	ProjectID   string  `json:"project_id,omitempty"`
	WorkspaceID string  `json:"workspace_id,omitempty"`
	Identity    string  `json:"identity"`
	TaskID      string  `json:"task_id,omitempty"`
	URL         *string `json:"url,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
}

// ResolveOrCaptureRequest selects an identity scope and supplies content for a new card.
type ResolveOrCaptureRequest struct {
	ProjectID   string  `json:"project_id,omitempty"`
	WorkspaceID string  `json:"workspace_id,omitempty"`
	Identity    string  `json:"identity,omitempty"`
	Title       string  `json:"title,omitempty"`
	Description string  `json:"description,omitempty"`
	URL         *string `json:"url,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
}

// ResolveOrCaptureResult identifies the card and whether this call created it.
type ResolveOrCaptureResult struct {
	TaskID  string `json:"task_id"`
	Created bool   `json:"created"`
}
