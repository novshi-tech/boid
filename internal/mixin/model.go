package mixin

import "github.com/novshi-tech/boid/internal/model"

// MixinMeta holds the parsed content of a mixin.yaml file.
type MixinMeta struct {
	TaskBehaviors      map[string]model.TaskBehavior `yaml:"task_behaviors"`
	Hooks              []model.Hook                  `yaml:"hooks"`
	HostCommands       []string                      `yaml:"host_commands"`
	AdditionalBindings []string                      `yaml:"additional_bindings"`
	Env                map[string]string             `yaml:"env"`
	AllowedDomains     []string                      `yaml:"allowed_domains"`

	// Set at load time, not from YAML.
	HooksDir string `yaml:"-"`
}
