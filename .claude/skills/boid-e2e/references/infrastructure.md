# E2E Infrastructure Overview

## run.sh Execution Flow

```
./e2e/run.sh [--keep-temp] [scenario]
```

1. Build the `boid` binary and `boid-e2e` helper with `go build`
2. Select the target scenario from `e2e/scenarios/` (runs all scenarios if no argument is given)
3. Create an isolated tmpdir for each scenario and do the following:
   - Set environment variables (HOME, XDG_DATA_HOME, BOID_SOCKET, etc.)
   - Copy fake commands from `e2e/fixtures/hostbin/` to `$E2E_BIN_DIR`
   - Copy fixture kits from `e2e/fixtures/kits/` to `$XDG_DATA_HOME/boid/kits/`
   - Copy the scenario's `workspace/` to `$E2E_WORKSPACE_DIR`
   - Start the boid server in the background
   - Wait for the server to be ready with `boid-e2e wait-health`
   - Execute `scenario.sh`
4. Stop the server and remove the tmpdir in the EXIT trap (retained on failure or with `--keep-temp`)

## e2e/lib/common.sh Helper Functions

| Function | Purpose |
|----------|---------|
| `e2e_log <msg>` | Prints `[e2e] <msg>` to stdout |
| `e2e_fail <msg>` | Prints `[e2e] ERROR: <msg>` to stderr and exits with code 1 |
| `e2e_require_cmd <cmd>` | Calls e2e_fail if the command is not found |
| `e2e_require_sandbox_prereqs` | Checks for the presence of pasta/unshare/nft (for scenarios that require sandbox) |
| `e2e_assert_contains <haystack> <needle>` | Calls e2e_fail if the string is not found |
| `e2e_run <cmd> ...` | Logs and then executes the command |
| `e2e_wait_for_file <path> [timeout] [interval]` | Waits until the file appears (default: 10s timeout, 0.05s interval) |

## boid-e2e Helper Commands

Test-only binary built from `e2e/cmd/boid-e2e/`.

| Command | Arguments | Description |
|---------|-----------|-------------|
| `wait-health [--timeout T] [--interval I] <socket>` | Socket path | Waits until /api/health returns ok |
| `get-task [--socket-path S] <task-id>` | Task ID | Retrieves task JSON and prints to stdout |
| `wait-task-status [--timeout T] [--interval I] [--socket-path S] <task-id> <status>` | Task ID, status | Waits until the task reaches the given status and prints JSON |
| `list-jobs [--socket-path S] <task-id>` | Task ID | Retrieves job list JSON for the task |
| `wait-job-count [--timeout T] [--interval I] [--socket-path S] <task-id> <count>` | Task ID, count | Waits until the job count reaches at least `count` |
| `assert-job-role-count [--socket-path S] <task-id> <role> <count>` | Task ID, role, count | Verifies the job count for the given role (exits with 1 if mismatched) |

**Role values**: `hook` (jobs from hooks), `gate` (jobs from gates)

## Environment Variables

Variables set by `run.sh` in the scenario subshell:

| Variable | Description |
|----------|-------------|
| `E2E_ROOT` | tmpdir root for scenario execution |
| `E2E_STATE_DIR` | `$E2E_ROOT/state` — log output directory for fake commands |
| `E2E_BIN_DIR` | `$E2E_ROOT/bin` — location of boid, boid-e2e, and fake commands |
| `E2E_LOG_DIR` | `$E2E_ROOT/logs` — server and scenario logs |
| `E2E_WORKSPACE_DIR` | `$E2E_ROOT/workspace` — copy destination for the project workspace |
| `BOID_SOCKET` | `$E2E_ROOT/run/boid.sock` — UNIX socket path for the boid server |
| `HOME` | `$E2E_ROOT/home` — isolated HOME directory |
| `PATH` | `$E2E_BIN_DIR:$PATH` — fake commands take priority |

## Sandbox Requirements

The following scenarios require a `requires-sandbox` marker:
- Scenarios that run inside sandbox (using the host command broker)
- Scenarios that require `pasta`, `unshare`, and `nft`

When the `requires-sandbox` file is present, `run.sh` calls `e2e_require_sandbox_prereqs`.
Use this for scenarios that can only run in CI (GitHub Actions).
