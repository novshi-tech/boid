#!/usr/bin/env bash
set -euo pipefail

RELEASE_FILE=".boid/release-verify-readonly"

while [[ ! -f "$RELEASE_FILE" ]]; do
  sleep 0.05
done

# When task.readonly=true the project directory is bind-mounted read-only.
# Verify by attempting a write; capture the result without failing the hook.
if touch ./boid-readonly-check 2>/dev/null; then
    rm -f ./boid-readonly-check
    FS_STATUS=writable
else
    FS_STATUS=readonly
fi

mkdir -p "$HOME/.boid/output"
printf '{"payload_patch":{"artifact":{"source":"verify-readonly","fs_status":"%s"}}}\n' \
    "$FS_STATUS" > "$HOME/.boid/output/payload_patch.yaml"
