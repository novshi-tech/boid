# Test Design Guidelines

## What to Test

### Happy Path (required)

- The task reaches the expected status (e.g., `done`)
- The artifact content is correct
- Project registration and task creation succeed

### State Transitions (when adding new features)

- The `pending` → `executing` → `done` transition occurs correctly
- How entry/exit gate firing timing and exit codes affect transitions
- Execution order of parallel and sequential hooks

### Error Cases (optional, may be omitted if it adds complexity)

- The exit gate fails and blocks the transition to done (remains in executing)
- Task interruption via abort action (`aborted` terminal state)

## Writing Assertions

Use `e2e_assert_contains <haystack> <needle>`.

```bash
# Save the task JSON response to a variable
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" done)"

# Assert status
e2e_assert_contains "$task_json" '"status":"done"'

# Assert artifact content
e2e_assert_contains "$task_json" '"artifact"'
e2e_assert_contains "$task_json" '"result":"done"'

# Assert project list
project_list="$("$E2E_BIN_DIR/boid" project list)"
e2e_assert_contains "$project_list" "my-scenario"
```

**Note**: `e2e_assert_contains` performs a substring match. Use it to check that JSON keys are correctly included.

## Async Wait Patterns

### Waiting for Task Status

```bash
# Poll at 100ms intervals for up to 20 seconds
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 20s --interval 100ms "$task_id" executing)"
```

Status values: `pending`, `executing`, `done`, `aborted`

### Waiting for Job Count

```bash
# Wait until at least 2 jobs exist (2 hooks have started)
"$E2E_BIN_DIR/boid-e2e" wait-job-count "$task_id" 2
```

### Verifying Job Count by Role

```bash
# Verify that there are 2 hooks and 0 gates
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" hook 2
"$E2E_BIN_DIR/boid-e2e" assert-job-role-count "$task_id" gate 0
```

### Waiting for a File to Appear

```bash
# Wait for the file written by agent-a after execution (function from e2e/lib/common.sh)
e2e_wait_for_file "$PROJECT_DIR/agent-a-instructions.json"
```

## Creating fake Commands (hostbin Pattern)

For scenarios that use host_commands, use fake scripts instead of the actual external commands (`gh`, `git`, `systemctl`, etc.).

### Placement

Place them in `e2e/fixtures/hostbin/`. `run.sh` automatically copies them to `$E2E_BIN_DIR` and prepends it to `$PATH`, so fake commands take priority.

### fake Script Template

```bash
#!/usr/bin/env bash
set -euo pipefail

log_file="${E2E_STATE_DIR:?}/fake-gh.log"
{
  printf 'cmd=gh\n'
  printf 'cwd=%s\n' "$PWD"
  printf 'args=%s\n' "$*"
  printf -- '---\n'
} >>"$log_file"

# Print to stdout as needed (to simulate the command's return value)
printf 'https://example.invalid/pr/123\n'

exit 0
```

### Verifying fake Logs

```bash
# Check that the expected command is recorded in fake-gh.log
[[ -f "$E2E_STATE_DIR/fake-gh.log" ]] || e2e_fail "missing fake gh log"
grep -F 'args=pr create --title My PR' "$E2E_STATE_DIR/fake-gh.log" \
  >/dev/null || e2e_fail "gh pr create was not invoked"
```

### Declaring host_commands in kit.yaml

```yaml
host_commands:
  gh:
    path: ${E2E_BIN_DIR}/gh   # fake command path
    allow:
      - pr                     # allowed subcommands
  systemctl:
    path: ${E2E_BIN_DIR}/systemctl
    allow:
      - restart
```

## Testing builtin Commands (boid / git)

When adding a new builtin op, include an E2E test that calls it from a hook/gate script via sandbox. Builtins that produce text output (like `boid task list`) behave differently when called as a host CLI from scenario.sh, so wire up a path that fires them from within sandbox.

Reference: `e2e/scenarios/builtin-task-create/` — demonstrates the pattern of calling `boid task create` from within a gate script.

## Handling Sandbox Prerequisites

Just place a `requires-sandbox` marker file (can be empty).

```bash
touch e2e/scenarios/my-scenario/requires-sandbox
```

This causes `run.sh` to check for the following:
- The `pasta` command is available
- The `unshare` command is available
- The `nft` command is available
- `unshare --user --mount --map-root-user` succeeds

**Scenarios requiring sandbox**: those that use hostcmd (the host command broker).
Scenarios without `requires-sandbox` can run on both CI and developer machines.

## Reference Scenarios

| Scenario | Characteristics | Reference Points |
|---------|-----------------|-----------------|
| `project-smoke` | Minimal setup, no sandbox required | Simple project registration + assertions |
| `readonly-hook-gate` | Parallel hooks + exit gate setup | hook/gate sync pattern |
| `phase-dependency` | Multiple child tasks with phase chaining via `artifact.children.all_done` | Parent-child / dependencies / abort behavior |
| `builtin-task-create` | `boid task create` builtin from hook/gate | Current pattern for subtask creation (replaces the old tasks trait) |
| `task-import-smoke` | Bulk import from JSONL (`boid task import`) | Bulk task submission |
| `host-command-smoke` | hostcmd (gh, systemctl), sandbox required | How to use fake commands |
