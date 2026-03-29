package mixin

import (
	"github.com/novshi-tech/boid/internal/hostcmd"
	"github.com/novshi-tech/boid/internal/model"
)

// MixinMeta holds the parsed content of a mixin.yaml file.
type MixinMeta struct {
	TaskBehaviors      map[string]model.TaskBehavior        `yaml:"task_behaviors"`
	Hooks              []model.Hook                         `yaml:"hooks"`
	HostCommands       map[string]hostcmd.CommandDef         `yaml:"host_commands"`
	AdditionalBindings []string                             `yaml:"additional_bindings"`
	Env                map[string]string                    `yaml:"env"`

	// Set at load time, not from YAML.
	HooksDir string `yaml:"-"`
}
