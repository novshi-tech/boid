#!/usr/bin/env bash
set -euo pipefail

# The release file is written by the host-side scenario.sh directly into
# the host project directory. Under the git gateway cutover (PR6), this
# sandbox's own view of the project is a fresh clone (not a live bind mount
# of the host dir), so a plain `[[ -f ... ]]` against a local relative path
# would never see it. fake-hook-cmd --check-file runs on the host (it's a
# broker-executed host_command) and checks the real host-side path instead.
RELEASE_FILE_HOST="${E2E_WORKSPACE_DIR:?}/app/.boid/release-verify-readonly"

while ! fake-hook-cmd --check-file "$RELEASE_FILE_HOST"; do
  sleep 0.05
done

# task.readonly's semantics changed under the git gateway cutover (PR6):
# readonly now means transport-RO (the gateway denies push/fetch-write),
# not filesystem-RO (docs/plans/git-gateway-cutover.md: "readonly の意味論
# 変更: FS-RO → transport-RO"). The sandbox-internal clone (under
# /workspace/<name>, see the workspace 親化リファクタリング) is
# always locally writable regardless of task.readonly, so this check is
# expected to observe "writable" even for a readonly task — that's the
# new, correct semantics, not a bug.
if touch ./boid-readonly-check 2>/dev/null; then
    rm -f ./boid-readonly-check
    FS_STATUS=writable
else
    FS_STATUS=readonly
fi

mkdir -p "$HOME/.boid/output"
printf '{"payload_patch":{"artifact":{"source":"verify-readonly","fs_status":"%s"}}}\n' \
    "$FS_STATUS" > "$HOME/.boid/output/payload_patch.json"
