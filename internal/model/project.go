package model

import "time"

type ProjectMeta struct {
	ID                 string                 `yaml:"id" json:"id"`
	WorkspaceID        string                 `yaml:"workspace_id" json:"workspace_id"`
	Name               string                 `yaml:"name" json:"name"`
	TaskBehaviors      map[string]TaskBehavior `yaml:"task_behaviors" json:"task_behaviors"`
	Hooks              []Hook                 `yaml:"hooks" json:"hooks"`
	HostCommands       []string               `yaml:"host_commands" json:"host_commands"`
	AdditionalBindings []string               `yaml:"additional_bindings" json:"additional_bindings"`
	Env                map[string]string      `yaml:"env" json:"env"`
	AllowedDomains     []string               `yaml:"allowed_domains" json:"allowed_domains"`
}

// Project represents a registered project.
// DB stores only ID, WorkDir, and timestamps.
// Meta is loaded from project.yaml at runtime via project.Store.
type Project struct {
	ID        string      `json:"id"`
	WorkDir   string      `json:"work_dir"`
	Meta      ProjectMeta `json:"meta"`
	CreatedAt time.Time   `json:"created_at"`
	UpdatedAt time.Time   `json:"updated_at"`
}
