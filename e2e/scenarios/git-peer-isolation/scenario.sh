#!/usr/bin/env bash
set -euo pipefail

# sandbox 内でワークスペースピアプロジェクトへの書き込みが阻止されることを確認する。
# broker cwd policy の単体テスト (internal/sandbox/git_builtin_test.go, policy_test.go)
# と合わせて「peer project への書き込みを構造的に塞ぐ」修正の E2E 検証となる。

APP_DIR="$E2E_WORKSPACE_DIR/app"
PEER_DIR="$E2E_WORKSPACE_DIR/peer"

# ピアプロジェクトのディレクトリが workspace テンプレートからコピーされていることを確認
[[ -d "$PEER_DIR" ]] || e2e_fail "peer project workspace dir not found: $PEER_DIR"

e2e_log "registering main project from $APP_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$APP_DIR"

e2e_log "registering peer project from $PEER_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PEER_DIR"

e2e_log "assigning both projects to the same workspace"
e2e_run "$E2E_BIN_DIR/boid" workspace assign git-peer-isolation     ws-isolation
e2e_run "$E2E_BIN_DIR/boid" workspace assign git-peer-isolation-peer ws-isolation

e2e_log "creating executor task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: git-peer-isolation
title: Git Peer Isolation Test
behavior: executor
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting task $task_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start

e2e_log "waiting for task completion"
task_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 30s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json"
e2e_assert_contains "$task_json" '"status":"done"'
e2e_assert_contains "$task_json" '"peer_write_rejected":true'

e2e_log "verified: peer project write was correctly rejected inside sandbox"
