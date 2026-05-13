# 2. Your first task

Before you wire up an AI agent, this page walks you through the bare task lifecycle. You will register one project, create a task, and watch it move through `pending → executing → done` from the CLI. It takes about five minutes.

This page assumes you have completed [1. Install](01-install.md).

## Why no agent yet

`boid` exists to run AI agents, but the agents live inside a structured task flow. Driving the flow once by hand — with no agent attached — makes it obvious what `boid` is doing for you. Later tutorials add kits, and you will be able to point at the same lifecycle and see what each kit takes care of.

## Set up a project

Pick any directory as your workspace.

```bash
mkdir -p ~/boid-demo
cd ~/boid-demo
```

Declare it as a `boid` project by writing `.boid/project.yaml`.

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

This is the smallest possible project file:

- `id: demo` — the identifier `boid` uses for this project.
- `task_behaviors.supervisor` — a single "kind of task". No hooks or gates are wired to it. `supervisor` is one of the two canonical behavior names; readonly is derived automatically from the name (supervisor ⇒ readonly), so we do not need to set it explicitly.

Register the project:

```bash
boid project add .
```

You should see something like `project added: demo`. Confirm with `boid project list`.

## Create a task

`boid task create` reads YAML from stdin.

```bash
boid task create <<'YAML'
project_id: demo
title: First task
behavior: supervisor
YAML
```

Note the ID it prints — referred to as `<task-id>` below.

Creating a task does not start any work. The status is `pending`:

```bash
boid task list
boid task show <task-id>
```

You will see `status: pending`.

## Drive the state machine

To move from `pending` to `executing`, send the `start` action.

```bash
boid action send --task <task-id> --type start
```

`task status: executing` comes back.

Because the `supervisor` behavior has no hooks attached in this minimal example, nothing actually runs in `executing`. `boid task show <task-id>` will keep showing `payload: {}`.

Write the `artifact` trait by hand — the trait a real hook would normally produce — and then send the `done` action to advance the task. Without a hook there is no `boid job done` event to drive the auto-transition, so we close the task manually.

```bash
echo '{"artifact":{"hello":"world"}}' \
  | boid task update <task-id> --payload-file -

boid action send --task <task-id> --type done
```

Look at the resulting status:

```bash
boid task show <task-id>
```

`status: done`. In a real workflow, a hook would write `artifact` and exit cleanly via `boid job done`, and the state machine would auto-transition `executing → done` for you.

## Inspect history

`boid` records every state change and every payload update. To list jobs that ran for this task:

```bash
boid job list --task <task-id>
```

It is empty here — no hooks were attached, so no jobs ran. Once you add a kit (next chapter), each handler invocation shows up as a job.

## Recap

What this tutorial introduced:

- Registering a **project** with `boid project add`.
- Declaring a **behavior** (with no handlers).
- Sending **actions** (`start` / `done`) to drive `pending → executing → done` manually.
- Writing a **payload patch** (`artifact`) to leave a result on the task.

In a real workflow these last two steps come from hooks invoking AI agents. The next tutorial introduces kits so `boid` can do the work for you.

## Cleanup

To remove what this tutorial created:

```bash
boid task delete <task-id>
boid project remove demo
rm -rf ~/boid-demo
```

---

Next: [3. Projects and extension packages (kits)](03-projects-and-kits.md)
