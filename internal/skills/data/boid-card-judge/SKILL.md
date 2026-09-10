---
name: boid-card-judge
description: Judge a manually created boid Card from its description, history, and work results. Use as the default metaproject's card-command judgment task, including asking the owner for missing requirements. Does not collect external signals.
---

# Advance the Card

Run inside the judgment task started by a Card command. Read `/boid-task` for
the task lifecycle and blocking `boid task ask` contract. Your task is the
judgment, not the work proposed for the Card: finish your task after recording
the judgment; completing your task does not mean completing the Card.

Read `boid card context` first. Its `card_id` identifies the Card; your own
`boid task current` identifies the judgment task. Read `boid card get <card_id>`
and `boid task show <card_id>` for the description, existing proposal, next-step
spec and history. Read relevant child results and the command's instruction.
Fetch `[attachment: <name>]` references with `boid task attachments get <name>`;
this reads your answer attachments and the source Card attachments.
The description can be the entire starting context: no signals, linked source,
repository or preconfigured business policy are required.

Determine what outcome the owner wants, what is already known, and the next
useful action. Use the owner's language. Preserve the original description;
write your synthesis through the summary helper below.

## Clarify when needed

If a missing requirement, target project, or completion criterion materially
changes the next action, call `boid task ask "<self-contained question>"`.
Explain the decision the answer will resolve; offer concrete alternatives when
helpful. Ask only for information not already in the Card, instruction, history,
or available workspace projects. Do not require every Card to pass an interview.

This call blocks and returns the answer. Keep it alive, following `/boid-task`'s
long-running command guidance. Continue this same judgment with the answer;
do not exit or create a second judge to wait for it. Record the relevant answer
in the Card summary so later judgments do not ask it again. An answer clarifies
the proposal; execution still goes through the Card's existing Go flow.

## Record the next step

Use the shared writer, with one JSON object on stdin:

```sh
python3 ~/.claude/skills/boid-metaproject/scripts/write.py <verb> < judgment.json
```

Read the writer's module documentation in
`~/.claude/skills/boid-metaproject/scripts/boidmeta/write.py` for payload fields.
Card-command calls omit `signals`; `card_write` from `boid card context` grants
writing even though the judgment task is readonly. Do not run Sweep or ack an
inbox, and do not reconstruct the writer's validation and recording logic.

- Write a concise `summary` of the goal, current facts, and useful answers.
- For executable work, inspect `boid project list` and `boid project show <id>`
  in this workspace. Choose an available project and behavior, write a `spec`
  with the work and completion criteria, then propose `go`. The built-in
  metaproject only judges; do not dispatch the proposed work back to its judge.
- If no suitable execution project exists, explain that and ask what the owner
  wants to do next. Do not invent a repository or silently create a project.
- For work the person must perform, propose `start` with a concrete action.
- Propose `park` only with a known wake condition, and `complete` only when the
  outcome is supported by the Card's facts or work results. Lack of information
  is a reason to clarify, not to discard or complete the Card.

Respect an existing next-step spec and running work. Read results before
proposing follow-up work; use the writer to replace obsolete reservations.
Record proposals rather than accepting them on the owner's behalf.

After successful recording, call `boid task notify --done` with a short account
of the judgment. On a technical failure, use the task failure path from
`/boid-task`; do not report success if the Card was not updated.
