#!/usr/bin/env bash
set -euo pipefail

PROJECT_DIR="$E2E_WORKSPACE_DIR/app"
PROJECT_YAML="$PROJECT_DIR/.boid/project.yaml"

e2e_log "registering project from $PROJECT_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PROJECT_DIR"

e2e_log "creating task with smoke-v1 behavior (initial)"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: project-reload
title: Smoke V1 Task
behavior: smoke-v1
YAML
)"
printf '%s\n' "$task_create_output"
e2e_assert_contains "$task_create_output" "task created"

e2e_log "rewriting project.yaml to add smoke-v2 behavior"
cat > "$PROJECT_YAML" <<'YAML'
id: project-reload
name: Project Reload
task_behaviors:
  smoke-v1:
    name: Smoke V1
    transition: one-shot
  smoke-v2:
    name: Smoke V2
    transition: one-shot
hooks: []
gates: []
YAML

e2e_log "reloading projects"
reload_output="$("$E2E_BIN_DIR/boid" project reload)"
printf '%s\n' "$reload_output"
e2e_assert_contains "$reload_output" "reload: ok"

e2e_log "creating task with smoke-v2 behavior (after reload)"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: project-reload
title: Smoke V2 Task
behavior: smoke-v2
YAML
)"
printf '%s\n' "$task_create_output"
e2e_assert_contains "$task_create_output" "task created"

e2e_log "project-reload scenario completed"
