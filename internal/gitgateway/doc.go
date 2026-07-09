// Package gitgateway implements the authenticating reverse proxy that sits
// between a sandbox's credential-less git and the real upstream forge
// (GitHub / Bitbucket). It is the enforcement half of the git gateway design
// in docs/plans/git-gateway-cutover.md ("gateway の認可モデル: 単一サーバ +
// job token の path prefix") and docs/plans/container-based-boid.md
// ("gateway の実現方式: Go 標準 ReverseProxy の自作薄層").
//
// This package (PR3 of the cutover plan) is self-contained and inert: it is
// not wired into internal/server or the dispatcher yet (that is PR4), and
// nothing in the running daemon constructs a Server today. It purposefully
// avoids importing internal/dispatcher or internal/db so a sandbox test run
// (which cannot build the sqlite-backed layers) can still build and test this
// package on its own — secret resolution and notification are expressed as
// small function-typed seams (SecretResolver, UpstreamAuthFailureNotifier)
// that PR4 adapts to the real internal/dispatcher.SecretStore and
// internal/notify.
package gitgateway
