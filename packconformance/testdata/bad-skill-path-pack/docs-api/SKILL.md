---
name: fixture-api
description: Conformance test NEGATIVE fixture for F2 — proves the skill
  scan follows manifest.Skills[].Path rather than a hardcoded "skills/"
  directory.
---

# Fixture API Reference (F2 regression fixture)

This file lives at `docs-api/SKILL.md`, not under any `skills/` directory
— on purpose. Run `boid task create` here to prove the scan actually
reached this file via the manifest's declared skills[].path.
