# Migrating repository configuration

Project configuration files are no longer read by the daemon. `boid project init` and `boid project migrate` have retired.

1. Export the destination workspace with `boid workspace export <slug>`.
2. Move behaviors, hooks, triggers, signal sources, card commands, and card events into the envelope's `spec`. Runtime environment settings belong there too.
3. Resolve duplicate names within that workspace; repository definitions have no precedence or fallback.
4. Apply the envelope using `boid workspace apply -f workspace.yaml` and inspect `boid project show <ref> --explain`.

Existing project IDs stay stable. New repository registrations derive IDs from the URL. `base_branch` accepts `${TASK_REMOTE_ID}`; `${current_branch}` has retired. Empty branches resolve to the origin default branch at agent checkout. Existing task branch snapshots are preserved.

Neither `project fetch` nor `project reload` imports configuration. Workspace changes are resolved at dispatch. See the [workspace configuration reference](../reference/project-yaml.md) for current fields and [onboarding](onboarding.md) for setup.
