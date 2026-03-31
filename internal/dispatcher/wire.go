package dispatcher

import (
	"github.com/novshi-tech/boid/internal/db"
	dtmux "github.com/novshi-tech/boid/internal/dispatcher/tmux"
	"github.com/novshi-tech/boid/internal/sandbox"
	"github.com/novshi-tech/boid/internal/secret"
)

type WireConfig struct {
	DB          *db.DB
	Tmux        dtmux.TmuxManager
	TmuxSession string
	Broker      *sandbox.Broker
	SecretStore *secret.Store
}

func Wire(cfg WireConfig) *Runner {
	return &Runner{
		DB:          cfg.DB,
		Tmux:        cfg.Tmux,
		TmuxSession: cfg.TmuxSession,
		Broker:      cfg.Broker,
		SecretStore: cfg.SecretStore,
	}
}
