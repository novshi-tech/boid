#!/usr/bin/env bash
set -euo pipefail

RELEASE_FILE=".boid/release-block-and-done"

# Block until the scenario releases us. The scenario stops/starts the daemon
# during this wait to exercise the auto-reopen path; on resume this hook is
# re-dispatched (new job) and waits at the same point until the release file
# appears.
while [[ ! -f "$RELEASE_FILE" ]]; do
  sleep 0.05
done

mkdir -p "$HOME/.boid/output"
printf '{"payload_patch":{"artifact":{"resumed":true}}}\n' > "$HOME/.boid/output/payload_patch.json"
