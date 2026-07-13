#!/usr/bin/env bash
set -euo pipefail

# docs/plans/git-gateway-cutover.md PR7b — 新規シナリオ 3/5:
# 「peer は fetch のみ」 (git-peer-clone-local の cutover 対応置換)。
#
# self (app) と peer (peer) の 2 project を 1 workspace に置き、self を
# executor task (writable) として dispatch する。この時 gateway registry の
# job token 許可集合は:
#   - self: PermFetchPush
#   - peer: PermFetch  (buildGatewayRepos の「workspace peers: PermFetch only」)
# となる。hook 内で:
#   (a) peer の clone URL は environment.yaml の workspace_projects[].clone_url
#       (peer advertise の post-cutover 形; sandbox_builder.go
#       buildEnvironmentYAML) から取れる
#   (b) その URL で peer を clone できる
#   (c) clone した peer への push は gateway 403 で拒否される
# を検証する (詳細は peer-fetch.sh のコメント参照)。scenario 側では加えて
# peer bare repo の main tip が hook 前後で動かないことを pin する。

APP_DIR="$E2E_WORKSPACE_DIR/app"
PEER_DIR="$E2E_WORKSPACE_DIR/peer"
PEER_UPSTREAM_BARE="$E2E_ROOT/upstream-repos/e2e-fixture/peer.git"

[[ -d "$APP_DIR"  ]] || e2e_fail "app project workspace dir not found: $APP_DIR"
[[ -d "$PEER_DIR" ]] || e2e_fail "peer project workspace dir not found: $PEER_DIR"
[[ -d "$PEER_UPSTREAM_BARE" ]] || e2e_fail "peer fixture upstream bare repo not found: $PEER_UPSTREAM_BARE"

peer_baseline_tip="$(/usr/bin/git -C "$PEER_UPSTREAM_BARE" rev-parse main)"
e2e_log "peer upstream main baseline tip: $peer_baseline_tip"

WS_SLUG="ws-peer-fetch"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - git-gateway-peer-fetch
YAML

e2e_log "registering self project from $APP_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$APP_DIR"
e2e_log "registering peer project from $PEER_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$PEER_DIR"

e2e_log "assigning both projects to the same workspace"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "git-gateway-peer-fetch"      "$WS_SLUG"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "git-gateway-peer-fetch-peer" "$WS_SLUG"

e2e_log "creating executor task on self (writable, but peer is fetch-only)"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: git-gateway-peer-fetch
title: Git Gateway Peer Fetch
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
e2e_assert_contains "$task_json" '"source":"peer-fetch"'
e2e_assert_contains "$task_json" '"peer_clone_url_seen":true'
e2e_assert_contains "$task_json" '"peer_marker_present":true'
e2e_assert_contains "$task_json" '"peer_push_denied":true'

peer_tip_after="$(/usr/bin/git -C "$PEER_UPSTREAM_BARE" rev-parse main)"
e2e_log "peer upstream main tip after hook: $peer_tip_after"
[[ "$peer_tip_after" == "$peer_baseline_tip" ]] || e2e_fail "peer upstream main advanced despite fetch-only policy (baseline=$peer_baseline_tip now=$peer_tip_after)"

e2e_log "verified: peer was fetchable via gateway; push to peer denied; peer upstream untouched"
