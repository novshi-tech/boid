// Package apigateway implements the authenticating reverse proxy that lets
// a sandboxed job reach configured HTTP APIs without ever holding the
// credential itself. See docs/plans/api-gateway.md for design background.
//
// The package is self-contained: it does not import internal/dispatcher,
// internal/db, internal/api, internal/server, or internal/sandbox. Secret
// resolution and notification/recording are expressed as small
// function-typed seams (SecretResolver, UpstreamAuthFailureNotifier,
// RequestRecorder) that callers adapt to the real
// internal/dispatcher.SecretStore, internal/notify, and the task timeline.
package apigateway
