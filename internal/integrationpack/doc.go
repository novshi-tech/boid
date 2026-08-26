// Package integrationpack implements the Integration Pack manifest schema,
// loader, and "脱糖" (desugaring) that turns a services.<name>.uses
// reference into the apigateway.ServiceConfig the API gateway already
// understands — docs/plans/signal-driven-review.md §6 ("Integration
// Pack")/§7 ("Service profile と service instance") and docs/plans/
// signal-ingest-detailed-design.md §6 ("Config と Pack loader"), PR-4.
//
// # Scope (PR-4)
//
// This package is deliberately narrow: manifest parsing/validation
// (manifest.go), directory enumeration (pack.go), the v0 minimal
// JSON-Schema subset a connector's declared config is checked against
// (configschema.go), and the profile→apigateway.ServiceConfig desugaring
// (resolve.go). It does NOT generate derived triggers, execute connectors,
// or mount Pack skills into a job sandbox — that is PR-5's job
// (docs/plans/signal-ingest-detailed-design.md §5), which consumes this
// package's *Pack/Manifest/DesugarService/ResolveServices as its
// foundation. Wiring LoadPacks+ResolveServices into the live daemon startup
// path (internal/server/wire.go's gateway wiring point) is also left to
// that follow-up PR — see the PR-4 submission notes for why.
//
// # Contract (Pack contract v1)
//
// docs/plans/signal-ingest-detailed-design.md §7 is the normative Pack
// contract every function/type here implements the boid-side half of:
//   - ManifestAPIVersion fixes the manifest schema version this package
//     understands (§7.3: boid never "前方互換を装う" — an unknown
//     apiVersion is a hard load error, not a best-effort parse).
//   - A service profile (ServiceProfile) declares only the SHAPE of a
//     connection (credential slot names + injection method, whether an
//     endpoint is configurable) — never a value (§7.1: "値 (endpoint 実値・
//     credential) を持たない"). Values live in config.yaml's services.<name>
//     entry (a "service instance") and are bound by DesugarService.
//   - v0 restricts a profile to AT MOST one credential slot and requires
//     the installed <version> directory name to match the manifest's own
//     metadata.version exactly (docs/plans/signal-ingest-detailed-design.md
//     §6.2's "先頭 slot を勝手に取るような縮退をしない") — both violations
//     are hard LoadPacks errors, never a silent best-guess.
//
// # Core/Pack layering (Q16/Q17)
//
// No file in this package imports anything service-specific (Jira/Slack/
// Bitbucket-...) — see TestQ16_NoServiceSpecificImports — and this package
// adds no database schema at all (it has no internal/db dependency; a
// loaded Pack registry lives only in daemon process memory, rebuilt from
// disk on every restart). Official Pack content (connectors/skills/
// resolvers) lives in a separate repo (boid-api-skills, evolving into the
// Pack repo — docs/plans/signal-driven-review.md §10's PR-8 note) and is
// never vendored into this one.
package integrationpack
