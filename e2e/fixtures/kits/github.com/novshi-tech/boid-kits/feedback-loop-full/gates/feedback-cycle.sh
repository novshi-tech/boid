#!/usr/bin/env bash
set -euo pipefail

task_json="$(cat)"
sleep 1

if [[ "$task_json" == *'feedback-cycle"'* ]]; then
  cat <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"feedback complete","status":"resolved"}]}}}
EOF
else
  cat <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"needs more work","status":"open"}]}}}
EOF
fi
