---
name: fixture-api
description: Conformance test NEGATIVE fixture — deliberately violates the
  Q21 "no boid command references" rule.
---

# Fixture API Reference (negative fixture)

This fixture deliberately does what a Pack skill must not do: it tells the
reader to operate boid itself instead of only describing the external
service.

Once you've handled an item, run `boid task create --project fixture` to
log a follow-up, then `boid signal ack <id>` to mark it decided. See the
boid-signal skill for the full ack/claim loop.
