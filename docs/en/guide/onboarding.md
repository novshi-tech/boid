# Workspace and repository onboarding

Create or update a workspace envelope with `boid workspace apply -f workspace.yaml` before launching tasks. Behaviors, hooks, triggers, signal sources, card commands, card events, and runtime settings all resolve from the workspace DB. Repositories need no boid configuration files.

See [Register a repository and configure its workspace](../getting-started/02-init-project.md) for a complete envelope. Register an existing remote using:

```bash
boid project add https://github.com/you/repo.git --workspace dev
boid project show repo --explain
boid workspace export dev
```

`--workspace` is required; the CLI creates an empty workspace if the slug is new. Apply task behaviors to it before task dispatch. `--name` supplies a repository name override. Existing project IDs remain stable; new IDs derive from the URL. Workspace settings apply at dispatch; `base_branch` is saved on each task at creation. An empty branch lets agent checkout use the origin default branch.

`project init` and `project migrate` have retired. Migrate any old repository definitions into `spec` of a workspace envelope and apply it. `project fetch` still refreshes mirror refs and `project reload` refreshes identities; neither reads project configuration.

Tool installation and authentication use the workspace's persistent HOME. See [workspace home](workspace-home.md).
