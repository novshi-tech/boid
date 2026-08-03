// Package apigateway implements the authenticating reverse proxy that lets
// a sandboxed job reach arbitrary configured HTTP APIs without ever holding
// the credential itself — the generalization of internal/gitgateway from
// git smart HTTP alone to any HTTP API, per docs/plans/api-gateway.md
// ("汎用 API gateway (認証注入リバースプロキシ) + OAuth2 対応 計画").
//
// PR1 added static credential injection (bearer / basic / header / query
// auth kinds). PR2 (this state) adds the oauth2 AuthKind's actual
// TokenSource (oauth2.go, OAuth2TokenSource): a daemon-single-refresher,
// proactively-refreshing (margin-before-expiry), singleflight-coalesced
// access-token supplier, wired into a CredentialProvider via
// SetOAuth2TokenSource. PR3 adds the `boid secret oauth login` flow that
// performs the initial grant; PR2's own dogfood path is a manually
// `boid secret set` refresh_token.
//
// Like internal/gitgateway, this package is self-contained: it does not
// import internal/dispatcher, internal/db, internal/api, internal/server,
// or internal/sandbox, so it (and a sandbox test run, which cannot build
// the sqlite-backed internal/db layers) can build and test on its own.
// Secret resolution and notification/recording are expressed as small
// function-typed seams (SecretResolver, UpstreamAuthFailureNotifier,
// RequestRecorder) that internal/server/internal/dispatcher adapt to the
// real internal/dispatcher.SecretStore, internal/notify, and the task
// timeline.
package apigateway
