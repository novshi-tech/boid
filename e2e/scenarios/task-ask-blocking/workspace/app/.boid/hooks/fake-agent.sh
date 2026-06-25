#!/usr/bin/env bash
set -euo pipefail

# Deterministic fake agent for the blocking Q&A (boid task ask) E2E test.
#
# `boid task ask` keeps THIS process alive and blocks inside the broker RPC
# until the user/supervisor answers. There is no second invocation: the same
# process unblocks with the answer on stdout and runs to completion, so the
# whole Q&A round-trip happens within a single hook job. (The legacy
# session-resume path that exited the agent and resumed it via a 2nd dispatch
# was removed in the session-id-resume cleanup.)

printf 'asking the magic word\n'

# Blocks here until `boid task answer` releases it. The broker fills the task id
# from the token context, and the daemon returns the answer on stdout. A direct
# invocation (no skill) is sufficient per the plan's PR3 notes.
answer="$(boid task ask 'What is the magic word?')"

printf 'got=%s\n' "$answer"

# Record the received answer in the artifact so the scenario can assert the
# blocking RPC delivered it end-to-end (the job log also captures the got= line).
mkdir -p "$HOME/.boid/output"
printf '{"payload_patch":{"artifact":{"received_answer":"%s"}}}\n' "$answer" \
  > "$HOME/.boid/output/payload_patch.json"
