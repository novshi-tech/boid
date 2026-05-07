#!/usr/bin/env bash
set -euo pipefail

# Deterministic fake agent for Q&A E2E testing.
#
# State machine driven by env vars injected by boid:
#   BOID_AGENT_SESSION_ID  empty on 1st invocation, non-empty on 2nd
#   BOID_USER_ANSWER       set to the user's answer on 2nd invocation
#   BOID_QUESTION_ID       Q&A turn ID set on 2nd invocation
#
# Log all relevant env vars for assertion in scenario.sh.
LOG="$E2E_STATE_DIR/fake-agent.log"
{
    printf 'BOID_AGENT_SESSION_ID=%s\n' "${BOID_AGENT_SESSION_ID:-}"
    printf 'BOID_USER_ANSWER=%s\n' "${BOID_USER_ANSWER:-}"
    printf 'BOID_QUESTION_ID=%s\n' "${BOID_QUESTION_ID:-}"
    printf '---\n'
} >> "$LOG"

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
    mkdir -p "$HOME/.boid/output"
    printf '{"payload_patch":{"artifact":{"status":"done"}}}\n' \
        > "$HOME/.boid/output/payload_patch.json"
    exit 0
fi

# Any answer other than "approve" is treated as rejection.
printf 'Rejected: %s\n' "${BOID_USER_ANSWER:-}" >&2
exit 1
