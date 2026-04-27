#!/usr/bin/env bash
set -euo pipefail

# hook から boid task create を呼ぶ (hook boid policy が task_create を許可)
# project_id は broker がコンテキストから自動注入する
boid task create <<'YAML'
title: Hook Spawned Task
behavior: child
ref: hook-spawned
YAML

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.yaml" <<'EOF'
{"payload_patch":{"artifact":{}}}
EOF
