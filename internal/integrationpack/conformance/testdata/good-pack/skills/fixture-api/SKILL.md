---
name: fixture-api
description: Conformance test fixture reference skill. Not a real API.
---

# Fixture API Reference

This is a minimal reference skill used only by
internal/integrationpack/conformance's own tests. It describes a made-up
"Fixture API": authenticate via the `$BOID_API_BASE` gateway contract
(credential injection is handled by the profile's declared slot, not by
this skill) and call `$BOID_API_BASE/<service>/v1/items`.

See [references/items.md](references/items.md) for the item schema.
