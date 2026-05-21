#!/usr/bin/env bash
set -euo pipefail

# Read parent TaskJSON from stdin and extract the id.
TASK_JSON="$(cat)"
PARENT_ID="$(printf '%s' "$TASK_JSON" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')"

boid task create >&2 <<EOF
title: Task A
behavior: child
ref: task-a
parent_id: $PARENT_ID
EOF

boid task create >&2 <<EOF
title: Task B
behavior: child
ref: task-b
parent_id: $PARENT_ID
EOF

boid task create >&2 <<EOF
title: Task C
behavior: child
ref: task-c
parent_id: $PARENT_ID
depends_on:
  - task-a
  - task-b
depends_on_payload: artifact.dummy
auto_start: true
EOF

boid task create >&2 <<EOF
title: Task D
behavior: child
ref: task-d
parent_id: $PARENT_ID
depends_on:
  - task-a
depends_on_payload: artifact.dummy
auto_start: true
EOF

# Emit artifact so the parent (one-shot) auto-advances to done.
mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<'EOF'
{"payload_patch":{"artifact":{"summary":"spawned subtasks"}}}
EOF
