#!/usr/bin/env bash
set -euo pipefail

# サンドボックス内でワークスペースピアプロジェクトを git clone --local で
# 参照できることを確認する。peer 本体は read-only マウントのため clone 後も
# HEAD が main のまま (untouched) であることも検証する。

APP_DIR="$E2E_WORKSPACE_DIR/app"
PEER_DIR="$E2E_WORKSPACE_DIR/peer"

# peer ディレクトリが存在することを確認
[[ -d "$PEER_DIR" ]] || e2e_fail "peer project workspace dir not found: $PEER_DIR"

# peer project を git repo として初期化 (E2E ではホスト上で実行)
e2e_log "initializing peer project as a git repository"
(
  cd "$PEER_DIR"
  /usr/bin/git init -b main
  /usr/bin/git config user.name "E2E Test"
  /usr/bin/git config user.email "e2e@boid.test"
  printf 'main content\n' > main.txt
  /usr/bin/git add main.txt
  /usr/bin/git commit -m "initial commit"
  /usr/bin/git checkout -b feature
  printf 'feature content\n' > feature.txt
  /usr/bin/git add feature.txt
  /usr/bin/git commit -m "feature commit"
  /usr/bin/git checkout main
)

# Set up workspace for new schema (PR4 hard cutover).
WS_SLUG="ws-clone-local"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - github.com/novshi-tech/boid-kits/git-peer-clone-local
YAML

e2e_log "registering main project from $APP_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$APP_DIR"

e2e_log "registering peer project from $PEER_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PEER_DIR"

e2e_log "assigning both projects to the same workspace"
e2e_run "$E2E_BIN_DIR/boid" workspace assign git-peer-clone-local      "$WS_SLUG"
e2e_run "$E2E_BIN_DIR/boid" workspace assign git-peer-clone-local-peer  "$WS_SLUG"

e2e_log "creating executor task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: git-peer-clone-local
title: Git Peer Clone Local Test
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
e2e_assert_contains "$task_json" '"clone_local_succeeded":true'

e2e_log "verified: peer project was cloned via --local and feature branch was accessible"
