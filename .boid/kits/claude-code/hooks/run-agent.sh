#!/bin/bash
set -e

TASK_TITLE=$(boid task get "$BOID_TASK_ID" --field title 2>/dev/null || echo "")
TASK_DESC=$(boid task get "$BOID_TASK_ID" --field description 2>/dev/null || echo "")

# BOID_INSTRUCTIONS ([]RoutedInstruction, role 昇順) + タスクコンテキストでプロンプトを構築
PROMPT=$(python3 - "$BOID_INSTRUCTIONS" "$TASK_TITLE" "$TASK_DESC" <<'PYEOF'
import json, sys

instructions_json = sys.argv[1] if len(sys.argv) > 1 else ""
task_title = sys.argv[2] if len(sys.argv) > 2 else ""
task_desc = sys.argv[3] if len(sys.argv) > 3 else ""

sections = []
if instructions_json:
    try:
        for inst in json.loads(instructions_json):
            sections.append("## " + inst["role"])
            sections.append(inst["message"])
    except (json.JSONDecodeError, KeyError, TypeError):
        pass

sections.append("## Task")
sections.append(task_title)
if task_desc:
    sections.append(task_desc)

print("\n\n".join(s for s in sections if s))
PYEOF
)

if [ -z "$PROMPT" ]; then
    echo "ERROR: no prompt" >&2
    exit 1
fi

exec claude --dangerously-skip-permissions -p "$PROMPT"
