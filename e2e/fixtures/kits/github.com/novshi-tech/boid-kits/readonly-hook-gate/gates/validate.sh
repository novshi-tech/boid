#!/usr/bin/env bash
set -euo pipefail
# Accept the artifact produced by the verify-readonly hook.
cat <<'EOF'
{}
EOF
