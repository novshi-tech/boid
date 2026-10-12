# Migrating repository configuration

Project configuration files are no longer read for runtime settings. `boid project init` and `boid project migrate` have retired.

1. Export the destination workspace with `boid workspace export <slug>`.
2. Move behaviors, hooks, triggers, signal sources, card commands, and card events into the envelope's `spec`. Runtime environment settings belong there too.
3. Resolve duplicate names within that workspace; repository definitions have no precedence or fallback.
4. Apply the envelope using `boid workspace apply -f workspace.yaml` and inspect `boid project show <ref> --explain`.

Existing project IDs stay stable. Migration 0062 reads the legacy `.boid/project.yaml` name once from the checkout or mirror HEAD and persists it in the DB. If the file is absent, unreadable, or malformed, it logs a warning when needed and uses the mirror basename. That fallback name remains fixed: there is no rename command. An envelope whose `projects:` entries still use the old name will no longer match. Update the envelope to the persisted name, or remove and re-add the registration with `--name=<old-name>` (URL-derived registration IDs may differ from legacy IDs; export and preserve affected tasks before removal). New registrations persist `--name` or the URL-derived name.

The daemon no longer clones or fetches mirrors. Migrations 0062 and 0064 perform one-time local reads; ordinary startup does not read mirrors. `project add` stores a required `upstream_url` and an empty `work_dir`; `project fetch` and `project reload` have retired. Existing mirror directories and `work_dir` values remain untouched for rollback. After a successful upgrade, operators may manually delete `<data-home>/repos/` mirrors once migrations 0062 and 0064 have completed and rollback to a binary that uses them is no longer needed. Back up the database and mirrors before deletion. Jobs create their own checkouts through the git gateway.

Repository cards now also receive the built-in `judge` command and card event defaults; workspace definitions can override them.

New repository registrations derive IDs from the URL. `base_branch` accepts `${TASK_REMOTE_ID}`; `${current_branch}` has retired. Empty branches resolve to the origin default branch at agent checkout. Existing task branch snapshots are preserved.

Workspace changes are resolved at dispatch. See the [workspace configuration reference](../reference/project-yaml.md) for current fields and [onboarding](onboarding.md) for setup.

Migration 0063 converts legacy `extra_repos` entries into `perm=fetch` registrations with empty `work_dir`. Existing registrations in the same workspace (or unassigned) are reused without lowering permissions. Repositories already assigned elsewhere receive a separate fetch registration for this workspace. Names that collide get a deterministic suffix and a warning; invalid entries are warned about and skipped. Export the workspace after upgrading to capture the actual names in `spec.projects`. Envelopes accept and ignore empty legacy `extra_repos: []` (or null); nonempty lists are rejected with migration guidance. The schema-version ceiling prevents an older binary from reopening the upgraded DB; rollback requires the pre-upgrade database backup.

Migration 0064 repairs NULL/empty `upstream_url` values by reading each legacy mirror's local `remote.origin.url` (`git config --local`), without fetching or contacting a forge. Valid forge URLs are normalized to HTTPS and saved on the existing project row, preserving IDs, tasks and workspace ownership. Existing URLs are unchanged. Missing mirrors/configuration and invalid or credential-bearing URLs produce a warning and are skipped without blocking startup. This migration runs once; keep the mirrors until it has completed, and inspect warnings before deleting them. Rows whose URL cannot be recovered still cannot run repository jobs; restore the pre-upgrade DB/mirror backup and correct the local origin before upgrading again, or export affected tasks before removing and re-registering the project.
