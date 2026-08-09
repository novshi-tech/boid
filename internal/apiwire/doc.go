// Package apiwire holds the daemon↔client wire contract: the request and
// response shapes both internal/api (server side) and internal/client +
// cmd (client side) encode and decode, plus the few validators that DEFINE
// a field's canonical form (NormalizePublicURL).
//
// It exists to break a compile-time dependency, not to reorganize code for
// its own sake. internal/client imported internal/api purely for these
// types, and internal/api is the daemon's handler package — so the import
// dragged internal/dispatcher, internal/db, internal/sandbox,
// internal/skills and web/templates in behind it. That made the CLI
// unbuildable for any GOOS but linux even though nothing on the client
// path needs a syscall the daemon owns (docs/plans/windows-client-build.md).
//
// The rule that keeps it working: apiwire may import internal/orchestrator
// (whose types appear in these payloads) and the standard library, and
// nothing else. TestApiwireDependencies pins it. internal/api re-exports
// every symbol here as an alias, so server-side code and web/templates are
// unaffected by the move.
package apiwire
