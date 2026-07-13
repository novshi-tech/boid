#!/usr/bin/env bash
set -euo pipefail

# docs/plans/git-gateway-cutover.md PR7b — 新規シナリオ 5/5:
# 「許可外 repo への push/fetch が gateway で 403」。
#
# 経路 (詳細は forbidden-repo.sh 参照):
#   workspace には 2 project dir が存在するが、scenario.sh は self (app)
#   だけを `boid project add` する。 unauthorized dir は
#   e2e_setup_fixture_upstream が bare upstream を seed するので上流には
#   存在するが、boid には未登録なので gateway registry の許可集合に
#   一切入らない。 self の job token を使って unauthorized の gateway URL
#   にアクセスすると fetch (ls-remote) / push とも 403 で拒否される。
#   加えて control として self 自身への fetch が通ることも確認する
#   (gateway 全体が壊れていて 403 とは別要因で拒否されているケースを分離)。
#
# scenario 側では加えて:
#   - unauthorized の bare upstream の main tip が hook 前後で動かないこと
#     (push が本当に届いていない証拠)

APP_DIR="$E2E_WORKSPACE_DIR/app"
UNAUTHORIZED_DIR="$E2E_WORKSPACE_DIR/unauthorized"
UNAUTHORIZED_BARE="$E2E_ROOT/upstream-repos/e2e-fixture/unauthorized.git"

[[ -d "$APP_DIR" ]]           || e2e_fail "self project dir not found: $APP_DIR"
[[ -d "$UNAUTHORIZED_DIR" ]]  || e2e_fail "unauthorized project dir not found: $UNAUTHORIZED_DIR"
[[ -d "$UNAUTHORIZED_BARE" ]] || e2e_fail "unauthorized fixture upstream bare repo not found: $UNAUTHORIZED_BARE"

unauthorized_baseline_tip="$(/usr/bin/git -C "$UNAUTHORIZED_BARE" rev-parse main)"
e2e_log "unauthorized upstream main baseline tip: $unauthorized_baseline_tip"

WS_SLUG="ws-forbidden-repo"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - git-gateway-forbidden-repo
YAML

# Deliberately register only the self project. The unauthorized dir is
# left unregistered so boid never adds it to any workspace / any job
# token's allow-list.
e2e_log "registering self project from $APP_DIR (unauthorized dir left unregistered)"
e2e_run "$E2E_BIN_DIR/boid" project add "$APP_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "git-gateway-forbidden-repo" "$WS_SLUG"

e2e_log "creating executor task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: git-gateway-forbidden-repo
title: Git Gateway Forbidden Repo
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
e2e_assert_contains "$task_json" '"source":"forbidden-repo"'
e2e_assert_contains "$task_json" '"fetch_denied":true'
e2e_assert_contains "$task_json" '"push_denied":true'
e2e_assert_contains "$task_json" '"control_fetch_ok":true'

unauthorized_tip_after="$(/usr/bin/git -C "$UNAUTHORIZED_BARE" rev-parse main)"
e2e_log "unauthorized upstream main tip after hook: $unauthorized_tip_after"
[[ "$unauthorized_tip_after" == "$unauthorized_baseline_tip" ]] || e2e_fail "unauthorized upstream main advanced despite 403 (baseline=$unauthorized_baseline_tip now=$unauthorized_tip_after)"

e2e_log "verified: gateway denied fetch and push for unregistered repo; self fetch still worked"
