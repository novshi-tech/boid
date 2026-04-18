package orchestrator

type PlannerWireConfig struct {
	Meta     MetaCache
	Projects ProjectCatalog
	Tasks    TaskLookup
}

func WireDispatchPlanner(cfg PlannerWireConfig) *DispatchPlanner {
	return &DispatchPlanner{
		Meta:     cfg.Meta,
		Projects: cfg.Projects,
		Tasks:    cfg.Tasks,
	}
}
