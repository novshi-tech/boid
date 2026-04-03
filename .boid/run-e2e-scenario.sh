#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

usage() {
  cat >&2 <<'EOF'
usage: boid-e2e <scenario>

Allowed scenarios:
  project-smoke
  host-command-smoke
  readonly-hook-gate
  writable-chain
  rework-cycle
  feedback-loop-full
EOF
}

if [[ $# -ne 1 ]]; then
  usage
  exit 2
fi

case "$1" in
  project-smoke|host-command-smoke|readonly-hook-gate|writable-chain|rework-cycle|feedback-loop-full)
    ;;
  *)
    printf 'unsupported scenario: %s\n' "$1" >&2
    usage
    exit 2
    ;;
esac

exec "$REPO_ROOT/e2e/run.sh" "$1"
