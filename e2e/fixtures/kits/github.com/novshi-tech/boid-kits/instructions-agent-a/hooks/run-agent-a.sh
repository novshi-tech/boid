#!/usr/bin/env bash
set -euo pipefail

# BOID_INSTRUCTIONS をプロジェクトルートに記録して artifact を emit
# (.boid/ は sandbox 内で read-only のため、プロジェクトルートに書く)
printf '%s' "${BOID_INSTRUCTIONS:-}" > "agent-a-instructions.json"

cat <<'EOF'
{"payload_patch":{"artifact":{"source":"agent-a"}}}
EOF
