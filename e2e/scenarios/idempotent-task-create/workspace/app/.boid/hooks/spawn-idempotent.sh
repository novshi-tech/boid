#!/usr/bin/env bash
set -euo pipefail

# Simulate supervisor resume: call boid task create twice with the same ref.
# The second call must return the same task ID (get-or-create idempotency).

id1=$(boid task create 2>/dev/null <<'YAML' | awk '{print $3}'
title: Step A
behavior: child
ref: step-a
YAML
)

id2=$(boid task create 2>/dev/null <<'YAML' | awk '{print $3}'
title: Step A
behavior: child
ref: step-a
YAML
)

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<EOF
{"payload_patch":{"artifact":{"first_id":"$id1","second_id":"$id2"}}}
EOF
