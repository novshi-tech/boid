package orchestrator

import "github.com/novshi-tech/boid/internal/adapters"

type PlannerWireConfig struct {
	Meta     MetaCache
	Projects ProjectCatalog
	Tasks    TaskLookup
	Adapter  adapters.HarnessAdapter
}

func WireDispatchPlanner(cfg PlannerWireConfig) *DispatchPlanner {
	return &DispatchPlanner{
		Meta:     cfg.Meta,
		Projects: cfg.Projects,
		Tasks:    cfg.Tasks,
		Adapter:  cfg.Adapter,
	}
}
