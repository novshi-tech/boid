package dispatcher

import (
	"database/sql"
)

type WireConfig struct {
	DB          *sql.DB
	Runtime     JobRuntime
	Broker      CommandBroker
	Sandbox     SandboxPreparer
	SecretStore *SecretStore
}

func Wire(cfg WireConfig) *Runner {
	return &Runner{
		DB:          cfg.DB,
		Runtime:     cfg.Runtime,
		Broker:      cfg.Broker,
		Sandbox:     cfg.Sandbox,
		SecretStore: cfg.SecretStore,
	}
}
