#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

# PR-4 (docs/plans/volume-only-daemon.md §論点e) removed the per-scenario
# black-box harness (e2e/run.sh + e2e/scenarios/*) this wrapper used to
# dispatch into: it was built around the userns backend + host-directory
# project registration, both removed in the same PR. e2e/run-container.sh
# is now the sole black-box E2E suite — a single fixed integration flow,
# not a per-scenario runner, so this wrapper no longer takes a scenario
# argument.
if [[ $# -gt 0 ]]; then
  printf 'note: run-e2e no longer takes a scenario argument (PR-4 removed the per-scenario harness); running the full container-backend suite (e2e/run-container.sh) instead. Ignoring argument(s): %s\n' "$*" >&2
fi

exec "$REPO_ROOT/e2e/run-container.sh"
