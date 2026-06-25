#!/usr/bin/env bash
set -euo pipefail

PARENT_ID="$BOID_TASK_ID"

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
auto_start: true
EOF

boid task create >&2 <<EOF
title: Task D
behavior: child
ref: task-d
parent_id: $PARENT_ID
auto_start: true
EOF

# Emit artifact so the parent (one-shot) auto-advances to done.
mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<'EOF'
{"payload_patch":{"artifact":{"summary":"spawned subtasks"}}}
EOF
