package orchestrator

type PlannerWireConfig struct {
	Meta         MetaCache
	Projects     ProjectCatalog
	Tasks        TaskLookup
	Worktrees    WorktreePreparer
	BoidBinary   string
	ServerSocket string
	ProxyPort    *int
}

func WireDispatchPlanner(cfg PlannerWireConfig) *DispatchPlanner {
	return &DispatchPlanner{
		Meta:         cfg.Meta,
		Projects:     cfg.Projects,
		Tasks:        cfg.Tasks,
		Worktrees:    cfg.Worktrees,
		BoidBinary:   cfg.BoidBinary,
		ServerSocket: cfg.ServerSocket,
		ProxyPort:    cfg.ProxyPort,
	}
}
