# 2. Initialize a project

This page walks you through initializing one **project** in `boid`. `boid` drives work as tasks, and every task belongs to a project. Setting one up first gives the tasks created in later tutorials somewhere to live. It takes about two minutes.

This page assumes you have completed [1. Install](01-install.md).

## What a project is

On disk a project is any directory that contains a `.boid/project.yaml`. It is typical for a project to correspond 1:1 with a repository, but a plain working directory is fine.

The minimum `.boid/project.yaml` declares the project's identifier (`id`) and the kinds of tasks the project can spawn (`task_behaviors`). Hooks, gates, and kits — which is where the actual work hangs off — are layered on top later.

## Create a workspace directory

Make a directory dedicated to this tutorial:

```bash
mkdir -p ~/boid-demo
cd ~/boid-demo
```

Dropping `.boid/project.yaml` into an existing repository works just as well.

## Write `.boid/project.yaml`

Create the smallest possible `project.yaml`:

```bash
mkdir .boid
cat > .boid/project.yaml <<'YAML'
id: demo
name: Demo
task_behaviors:
  supervisor:
    name: Supervisor
YAML
```

What each field means:

- **`id: demo`** — the identifier `boid` uses internally. Tasks reference it via `project_id: demo` when you call `boid task create`.
- **`name: Demo`** — the human-readable label shown in the Web UI and TUI.
- **`task_behaviors.supervisor`** — declares one kind of task this project can spawn. `supervisor` is one of the two canonical behavior names; readonly is derived automatically from the name (supervisor ⇒ readonly), so we do not need to set it explicitly.

In real use you would wire hooks, gates, or kits to the behavior so it launches an AI agent or opens a sandbox. We deliberately keep this minimal here; [4. Set up a kit](04-kits.md) adds a kit that drives Claude Code on top of this project. `boid`'s architecture is intentionally agent-neutral, but at the moment Claude Code is the only agent with production-grade support.

## Register the project

Tell the daemon about the project:

```bash
boid project add .
```

You should see `project added: demo`. The `.` points at the current directory (`~/boid-demo`); the daemon reads the `.boid/project.yaml` underneath it and ingests the contents.

List registered projects:

```bash
boid project list
```

Show the details:

```bash
boid project show demo
```

You should see `id`, `name`, and `task_behaviors` reflected.

## When you edit `project.yaml`

`project.yaml` is loaded into the daemon at registration time. After editing the file, reload every project with:

```bash
boid project reload
```

You do not need to restart the daemon; in-flight tasks are not affected.

## Local overrides (`project.local.yaml`)

Settings you do not want to commit to the repository (personal extra bindings, environment variables) can be layered on via `.boid/project.local.yaml`. Generate a skeleton with:

```bash
boid project local init
```

See the [`project.yaml` reference](../reference/project-yaml.md) for details. This tutorial does not use it; knowing it exists is enough for now.

## Recap

What this tutorial introduced:

- Declaring `id` and `task_behaviors` in `.boid/project.yaml`.
- Registering the project with the daemon via `boid project add`.
- Inspecting the registration with `boid project list` / `show`.
- Reloading edits with `boid project reload`.

The next tutorial uses this project to set up the Web UI before running a task against it.

## Cleanup (optional)

To remove what this tutorial created:

```bash
boid project remove demo
rm -rf ~/boid-demo
```

The next tutorial reuses this project though, so leave it in place if you plan to keep reading.

---

Next: [3. Set up the Web UI](03-web-ui.md)
