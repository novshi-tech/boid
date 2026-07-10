#!/usr/bin/env bash
set -euo pipefail

# The release file is written by the host-side scenario.sh directly into
# the host project directory. Under the git gateway cutover (PR6), this
# sandbox's own view of the project is a fresh clone (not a live bind mount
# of the host dir), so a plain `[[ -f ... ]]` against a local relative path
# would never see it. fake-hook-cmd --check-file runs on the host (it's a
# broker-executed host_command) and checks the real host-side path instead.
RELEASE_FILE_HOST="${E2E_WORKSPACE_DIR:?}/app/.boid/release-block-and-done"

# Block until the scenario releases us. The scenario stops/starts the daemon
# during this wait to exercise the auto-reopen path; on resume this hook is
# re-dispatched (new job) and waits at the same point until the release file
# appears.
while ! fake-hook-cmd --check-file "$RELEASE_FILE_HOST"; do
  sleep 0.05
done

mkdir -p "$HOME/.boid/output"
printf '{"payload_patch":{"artifact":{"resumed":true}}}\n' > "$HOME/.boid/output/payload_patch.json"
