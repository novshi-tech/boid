package dispatcher

import (
	"database/sql"

	dtmux "github.com/novshi-tech/boid/internal/dispatcher/tmux"
	"github.com/novshi-tech/boid/internal/sandbox"
)

type WireConfig struct {
	DB          *sql.DB
	Tmux        dtmux.TmuxManager
	TmuxSession string
	Broker      *sandbox.Broker
	SecretStore *SecretStore
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
