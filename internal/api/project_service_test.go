package api

// Reserved for PR4: the workspaces-row existence check that codex MAJOR 5
// asked for (and PR3's first pass implemented) had to be reverted after
// e2e uncovered that every existing "yaml on disk → `boid workspace
// assign`" flow (docker-proxy-* scenarios, daemon-restart-resume) breaks
// against it — the assign runs against a live daemon whose migration has
// already committed, so a slug backed only by a freshly-dropped yaml file
// has no workspaces row.
//
// The check will come back in PR4 alongside the create path (`POST
// /api/workspaces` + CLI counterpart) that lets callers introduce a new
// workspaces row outside the migration path. The rejection / default /
// exists / empty-clear tests that lived in this file (against a
// stubProjectRepository.existingWorkspaces map) will be reintroduced then.
