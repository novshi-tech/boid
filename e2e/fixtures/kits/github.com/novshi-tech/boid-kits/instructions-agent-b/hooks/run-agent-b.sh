#!/usr/bin/env bash
set -euo pipefail

# verifying フェーズは read-only のためファイル書き込みは行わず、
# resolved verification を emit して in_review への遷移を促す
mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"review passed","status":"resolved"}]}}}
EOF
