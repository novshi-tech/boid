#!/usr/bin/env bash
set -euo pipefail

# broker 経由で hostcmd を呼ぶ (hook policy が Gate と同等であることの検証)
fake-hook-cmd --hook-ran

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<'EOF'
{"payload_patch":{"artifact":{"branch":"boid/e2e","commit":"deadbeef"}}}
EOF
