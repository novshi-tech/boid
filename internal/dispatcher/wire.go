package dispatcher

import (
	"database/sql"

	dtmux "github.com/novshi-tech/boid/internal/dispatcher/tmux"
)

type WireConfig struct {
	DB          *sql.DB
	Tmux        dtmux.TmuxManager
	TmuxSession string
	Broker      CommandBroker
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
