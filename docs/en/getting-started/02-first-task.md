# 2. Your first task

Before you wire up an AI agent, this page walks you through the bare task lifecycle. You will register one project, create a task, and watch it move through `pending → executing → verifying → done` from the CLI. It takes about five minutes.

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
  hello:
    name: Hello
    readonly: true
YAML
```

This is the smallest possible project file:

- `id: demo` — the identifier `boid` uses for this project.
- `task_behaviors.hello` — a single "kind of task". No hooks or gates are wired to it.
- `readonly: true` — declares that the sandbox for this task should be read-only. Since we have no scripts to run anyway, this just records "no side effects expected".

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
behavior: hello
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

Because `hello` has no hooks attached, nothing actually runs in `executing`. `boid task show <task-id>` will keep showing `payload: {}`.

So let's do by hand what a hook would normally do — write the trait that signals "executing is finished". As described in [Concepts](../guide/concepts.md#payload-and-traits), once `artifact` is present in the payload, `boid` treats `executing` as complete and advances the task automatically.

```bash
echo '{"artifact":{"hello":"world"}}' \
  | boid task update <task-id> --payload-file -
```

Look at the resulting status:

```bash
boid task show <task-id>
```

`status: done`. The chain was:

1. `artifact` got written → `executing → verifying`
2. No verifying-state handler exists → falls straight through to `done`

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
- Sending an **action** (`start`) for a manual `pending → executing` transition.
- Writing a **payload patch** (`artifact`) and watching the automatic `executing → verifying → done` transitions.

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
