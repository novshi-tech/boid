// Package integrationpack implements the Integration Pack manifest schema,
// loader, and "脱糖" (desugaring) that turns a services.<name>.uses
// reference into the apigateway.ServiceConfig the API gateway already
// understands.
//
// # Scope
//
// This package is deliberately narrow: manifest parsing/validation
// (manifest.go), directory enumeration (pack.go), the v0 minimal
// JSON-Schema subset a connector's declared config is checked against
// (configschema.go), and the profile→apigateway.ServiceConfig desugaring
// (resolve.go). It does NOT generate derived triggers, execute connectors,
// or mount Pack skills into a job sandbox — a later package consumes this
// one's *Pack/Manifest/DesugarService/ResolveServices as its foundation.
// Wiring LoadPacks+ResolveServices into the live daemon startup path
// (internal/server/wire.go's gateway wiring point) is also left to that
// follow-up.
//
// # Contract (Pack contract v1)
//
// Every function/type here implements the boid-side half of the Pack
// contract:
//   - ManifestAPIVersion fixes the manifest schema version this package
//     understands — an unknown apiVersion is a hard load error, not a
//     best-effort parse (boid never pretends forward compatibility).
//   - A service profile (ServiceProfile) declares only the SHAPE of a
//     connection (credential slot names + injection method, whether an
//     endpoint is configurable) — never a value. Values live in
//     config.yaml's services.<name> entry (a "service instance") and are
//     bound by DesugarService.
//   - v0 restricts a profile to AT MOST one credential slot and requires
//     the installed <version> directory name to match the manifest's own
//     metadata.version exactly — both violations are hard LoadPacks
//     errors, never a silent best-guess.
//
// # Core/Pack layering
//
// No file in this package imports anything service-specific (Jira/Slack/
// Bitbucket-...) — see TestQ16_NoServiceSpecificImports — and this package
// adds no database schema at all (it has no internal/db dependency; a
// loaded Pack registry lives only in daemon process memory, rebuilt from
// disk on every restart). Official Pack content (connectors/skills/
// resolvers) lives in a separate repo and is never vendored into this one.
package integrationpack
