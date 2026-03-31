package kit

import (
	"github.com/novshi-tech/boid/internal/hostcmd"
	"github.com/novshi-tech/boid/internal/model"
)

// KitMeta holds the parsed content of a kit.yaml file.
type KitMeta struct {
	TaskBehaviors      map[string]model.TaskBehavior        `yaml:"task_behaviors"`
	Hooks              []model.Hook                         `yaml:"hooks"`
	Gates              []model.Gate                         `yaml:"gates"`
	HostCommands       map[string]hostcmd.CommandDef         `yaml:"host_commands"`
	AdditionalBindings []model.BindMount                    `yaml:"additional_bindings"`
	Env                map[string]string                    `yaml:"env"`

	// Set at load time, not from YAML.
	HooksDir string `yaml:"-"`
	GatesDir string `yaml:"-"`
}
