package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/notify"
)

// gatewayNotifier adapts internal/notify.Service to
// gitgateway.UpstreamAuthFailureNotifier, giving upstream auth failures and
// credential-injection failures distinct, remediation-oriented messages.
//
// A nil notify field makes both methods a no-op, matching notify.Service's
// own nil-receiver convention.
type gatewayNotifier struct {
	notify *notify.Service
}

// NotifyUpstreamAuthFailure implements gitgateway.UpstreamAuthFailureNotifier.
func (n gatewayNotifier) NotifyUpstreamAuthFailure(host string, repo gitgateway.RepoKey) {
	n.send(fmt.Sprintf(
		"git gateway: upstream %s rejected credentials for %s (401) — the configured token may be expired or revoked; rotate it with `boid secret set`",
		host, repo))
}

// NotifyCredentialError implements gitgateway.UpstreamAuthFailureNotifier.
func (n gatewayNotifier) NotifyCredentialError(host string, repo gitgateway.RepoKey, err error) {
	n.send(fmt.Sprintf(
		"git gateway: could not inject credentials for %s (host %s): %v — check config.yaml's gateway.hosts entry and the referenced secret",
		repo, host, err))
}

func (n gatewayNotifier) send(msg string) {
	if n.notify == nil {
		return
	}
	if err := n.notify.Notify(context.Background(), notify.Event{Message: msg}); err != nil {
		slog.Warn("git gateway: notify failed", "error", err)
	}
}
