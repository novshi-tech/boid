---
name: boid-e2e
description: >
  Provides guides, templates, and design principles for creating and adding new E2E scenarios to the boid project.
  Covers scenario directory structure, project.yaml/scenario.sh templates, assertion patterns,
  and how to use fake commands.
  Use when a team member wants to add a new E2E scenario (e.g. "add an e2e scenario",
  "add a regression test for the new feature", "create a new scenario under e2e/scenarios/")
  or when implementing a new boid feature that requires end-to-end test coverage.
---

# boid E2E Test Creation Skill

Step-by-step guide for creating E2E tests alongside new features.
Refer to `e2e/scenarios/project-smoke` (the simplest example) as a reference for existing scenarios.

## Quick Start

```
1. Create the e2e/scenarios/<scenario-name>/ directory
2. Place workspace/app/.boid/project.yaml
3. Create scenario.sh
4. (If sandbox is required) Place requires-sandbox
5. Verify with ./e2e/run.sh <scenario-name> (runs on host only)
```

## Detailed Reference

- [E2E Infrastructure Overview](references/infrastructure.md) — run.sh execution flow, helper functions, and environment variables
- [Scenario Creation Template](references/scenario-template.md) — directory structure, project.yaml, and scenario.sh templates
- [Test Design Guidelines](references/design-guidelines.md) — what to test, assertions, wait patterns, and fake commands

## Checklist

Verify the following after creating a new scenario:

- [ ] `e2e/scenarios/<name>/scenario.sh` is created
- [ ] `e2e/scenarios/<name>/workspace/app/.boid/project.yaml` has the correct structure
- [ ] At least one happy-path assertion (`e2e_assert_contains`) is present
- [ ] `requires-sandbox` file is present if sandbox is required
- [ ] fixture kit (`e2e/fixtures/kits/`) is added if needed
- [ ] Unit tests are not broken (verify with `go test ./...`)
- [ ] Existing scenarios are not broken (rely on CI or run all scenarios with `./e2e/run.sh`)
