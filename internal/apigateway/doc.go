// Package apigateway implements the authenticating reverse proxy that lets
// a sandboxed job reach arbitrary configured HTTP APIs without ever holding
// the credential itself — the generalization of internal/gitgateway from
// git smart HTTP alone to any HTTP API, per docs/plans/api-gateway.md
// ("汎用 API gateway (認証注入リバースプロキシ) + OAuth2 対応 計画").
//
// This is PR1 of that plan: static credential injection only (bearer /
// basic / header / query auth kinds). The oauth2 AuthKind is parsed and
// validated by config schema (internal/config) but Resolve/Inject both
// return an explicit "not implemented yet" error for it — PR2 adds the
// actual TokenSource.
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
