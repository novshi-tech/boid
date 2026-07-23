# Scenario Creation Template

## Directory Structure

```
e2e/scenarios/<scenario-name>/
├── scenario.sh                    # scenario script (required)
├── requires-sandbox               # only if sandbox is required (empty file)
└── workspace/
    └── app/
        └── .boid/
            └── project.yaml       # project definition for testing (required)
```

If a fixture kit (custom hooks/gates) is needed:

```
e2e/fixtures/kits/github.com/novshi-tech/boid-kits/<kit-name>/
├── kit.yaml
├── hooks/
│   └── <hook-id>.sh
└── gates/
    └── <gate-id>.sh
```

## project.yaml Templates

### Minimal Setup (no hooks or gates)

Reference: `e2e/scenarios/project-smoke/workspace/app/.boid/project.yaml`

```yaml
id: my-scenario
name: My Scenario
task_behaviors:
  smoke:
    name: Smoke
hooks: []
gates: []
```

### Setup with Kit (with exit gate)

Reference: `e2e/scenarios/readonly-hook-gate/workspace/app/.boid/project.yaml`

```yaml
id: my-scenario
name: My Scenario
kits:
  - github.com/novshi-tech/boid-kits/<kit-name>
task_behaviors:
  dev:
    name: Dev
hooks: []
gates: []
```

### kit.yaml Template

```yaml
env:
  E2E_STATE_DIR: ${E2E_STATE_DIR}   # inject environment variables as needed
hooks:
  - id: my-hook                      # always starts in executing state (the on: field is deprecated)
gates:
  - id: my-gate
    phase: exit                      # entry (just before pending → executing) or exit (just before executing → done)
    traits:
      consumes: [artifact]           # declare traits to access
```

**hooks**: Always start in the `executing` state (the `on:` field is deprecated).
**gates**: Fire just before entering the next state with `phase: entry`, or just before leaving the current state with `phase: exit`. Defaults to `exit` if omitted.

## scenario.sh Templates

### Basic Pattern (register project → create task → verify)

Reference: `e2e/scenarios/project-smoke/scenario.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"

# 1. Register the project
e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

# 2. Create the task
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: my-scenario
title: My Test Task
behavior: smoke
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

# 3. Start the task
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

# 4. Wait for completion and verify
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 15s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'
```

### hook/gate Sync Pattern (file-based release control)

Reference: `e2e/scenarios/readonly-hook-gate/scenario.sh`

```bash
# hook script: block until the file appears
while [[ ! -f ".boid/release-my-hook" ]]; do sleep 0.05; done

# scenario: verify the hook has started before releasing
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 1
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 1
touch "$PROJECT_DIR/.boid/release-my-hook"
```

### Creating a Task with Payload Override

Reference: `e2e/scenarios/instructions-routing/scenario.sh`

```bash
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: my-scenario
title: My Task
behavior: dev
payload:
  instructions:
    executor:
      type: execution
      consumer: claude-code
      message: "implement the feature"
YAML
)"
```

## hook/gate Script Templates

### hook Script (outputting artifact)

```bash
#!/usr/bin/env bash
set -euo pipefail

# Blocking (optional)
while [[ ! -f ".boid/release-my-hook" ]]; do sleep 0.05; done

# Apply the payload patch immediately via the broker RPC (preferred —
# $HOME/.boid/output/payload_patch.json is retired, no longer read).
boid task update --payload-patch @- <<'EOF'
{"artifact":{"result":"done"}}
EOF
```

### gate Script (outputting verification to stdout)

```bash
#!/usr/bin/env bash
set -euo pipefail

# Write JSON to stdout (directly to stdout, not payload_patch)
cat <<'EOF'
{"payload_patch":{"verification":{"findings":[{"message":"all checks passed","status":"resolved"}]}}}
EOF
```
