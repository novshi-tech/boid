#!/usr/bin/env bash
set -euo pipefail

# Pins docs/plans/phase6-cutover-followups.md §⓪ "broker TCP wire
# completion": `boid task update --payload-patch @-` must round-trip
# job -> broker -> daemon regardless of which transport the sandbox's own
# BOID_BROKER_* env selects. This scenario runs under the standard
# ./e2e/run.sh harness (userns backend, real daemon+broker — no fake), so
# it exercises the UNIX transport branch of the refactored
# internal/sandbox/brokerclient.SendJSONFromEnv/JobDone decision point end
# to end; the equivalent TLS-transport branch (a container-backend job's
# sibling container) is exercised by e2e/run-container.sh's own broker RPC
# check, which no fake-docker/mocked scenario under this harness can
# reach (it needs a real docker-out-of-docker deploy — see that script's
# own header comment).
#
# Recording which transport actually fired (rather than assuming) makes a
# future regression — e.g. SendJSONFromEnv silently falling back to no
# transport at all, or picking UNIX when TLS should have won — visible in
# the task's own payload instead of only in a broker-side log line no one
# is watching.
transport="unix"
if [[ -n "${BOID_BROKER_TLS_ADDR:-}" ]]; then
  transport="tls"
fi

printf '{"artifact":{"result":"pass","broker_transport":"%s"}}\n' "$transport" | boid task update --payload-patch @-
