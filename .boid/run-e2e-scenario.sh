#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

if [[ $# -ne 1 ]]; then
  printf 'usage: boid-e2e <scenario>\n' >&2
  exit 2
fi

scenario_dir="$REPO_ROOT/e2e/scenarios/$1"
if [[ ! -d "$scenario_dir" ]] || [[ ! -f "$scenario_dir/scenario.sh" ]]; then
  printf 'unknown scenario: %s\n' "$1" >&2
  exit 2
fi

exec "$REPO_ROOT/e2e/run.sh" "$1"
