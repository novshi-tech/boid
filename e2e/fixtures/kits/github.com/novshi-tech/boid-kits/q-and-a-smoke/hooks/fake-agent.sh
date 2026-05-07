#!/usr/bin/env bash
set -euo pipefail

# Deterministic fake agent for Q&A E2E testing.
#
# State machine driven by env vars injected by boid:
#   BOID_AGENT_SESSION_ID  empty on 1st invocation, non-empty on 2nd
#   BOID_USER_ANSWER       set to the user's answer on 2nd invocation
#   BOID_QUESTION_ID       Q&A turn ID set on 2nd invocation
#
# Env var correctness is verified indirectly via state transitions:
# if BOID_AGENT_SESSION_ID were missing, the 2nd invocation would re-enter
# the notify --ask path instead of acting on BOID_USER_ANSWER, causing the
# task to re-enter awaiting rather than reach done/aborted.

if [[ -z "${BOID_AGENT_SESSION_ID:-}" ]]; then
    # First invocation: signal that the plan is ready and ask for approval.
    printf 'Plan ready\n'
    boid task notify "$BOID_TASK_ID" \
        --message "Plan ready, awaiting approval" \
        --ask "Approve?" \
        --session-id "fake-session-xyz" \
        --question-id "q-approve-001"
    exit 0
fi

# Second invocation: act on the user's answer.
if [[ "${BOID_USER_ANSWER:-}" == "approve" ]]; then
    printf 'Done\n'
    exit 0
fi

# Any answer other than "approve" is treated as rejection.
printf 'Rejected: %s\n' "${BOID_USER_ANSWER:-}" >&2
exit 1
