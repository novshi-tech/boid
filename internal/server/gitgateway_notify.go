package server

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/novshi-tech/boid/internal/gitgateway"
	"github.com/novshi-tech/boid/internal/notify"
)

// gatewayNotifier adapts internal/notify.Service to
// gitgateway.UpstreamAuthFailureNotifier, giving each of the two failure
// modes its own remediation-oriented message (docs/plans/git-gateway-cutover.md
// PR4, flagged in PR3's review: 「config error (認証注入失敗) と token 失効を
// differentiate できると良い」):
//
//   - an upstream 401 means the configured token itself is wrong ("rotate
//     it")
//   - a credential-injection failure means the gateway config/secret store
//     reference is wrong ("fix config.yaml / boid secret set")
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
