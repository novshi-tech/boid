package orchestrator

import "github.com/novshi-tech/boid/internal/adapters"

type PlannerWireConfig struct {
	Meta     MetaCache
	Hydrator MetaHydrator // optional; when set, dispatch uses GetWithWorkspace
	Projects ProjectCatalog
	Tasks    TaskLookup
	Adapter  adapters.HarnessAdapter
}

func WireDispatchPlanner(cfg PlannerWireConfig) *DispatchPlanner {
	return &DispatchPlanner{
		Meta:     cfg.Meta,
		Hydrator: cfg.Hydrator,
		Projects: cfg.Projects,
		Tasks:    cfg.Tasks,
		Adapter:  cfg.Adapter,
	}
}
