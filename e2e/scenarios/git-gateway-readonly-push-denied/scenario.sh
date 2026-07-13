#!/usr/bin/env bash
set -euo pipefail

# docs/plans/git-gateway-cutover.md PR7b — 新規シナリオ 2/5:
# 「readonly job の push が拒否される」。
#
# 期待する経路: supervisor task (readonly=true; canonical behavior の
# fail-safe default、behavior_resolve.go の applyCanonicalBehaviorOverrides
# 参照) を dispatch すると、gateway registry は self project について
# PermFetch のみを job token に紐付ける (gitgateway_wire.go
# buildGatewayRepos: `perm := gitgateway.PermFetch;
# if spec.Visibility.Writable { perm = gitgateway.PermFetchPush }`)。
# よって hook 内で git push すると gateway が 403 を返して失敗する
# (server.go の "forbidden: repo/operation not permitted for this job token")。
# 同じ token で fetch は許可されるため `git fetch origin main` は通る。
#
# アサーション:
#   - task が done で終わる (hook が全項目 OK で exit 0 した)
#   - hook が payload_patch.push_denied=true / fetch_ok=true を書いている
#   - fixture upstream bare repo の main tip は harness 起動時の baseline
#     から動いていない (push が本当に届いていない証拠)

APP_DIR="$E2E_WORKSPACE_DIR/app"
UPSTREAM_BARE="$E2E_ROOT/upstream-repos/e2e-fixture/app.git"

[[ -d "$UPSTREAM_BARE" ]] || e2e_fail "fixture upstream bare repo not found: $UPSTREAM_BARE"

baseline_tip="$(/usr/bin/git -C "$UPSTREAM_BARE" rev-parse main)"
e2e_log "upstream main baseline tip: $baseline_tip"

WS_SLUG="ws-readonly-push"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - git-gateway-readonly-push-denied
YAML

e2e_log "registering project from $APP_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$APP_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "git-gateway-readonly-push-denied" "$WS_SLUG"

e2e_log "creating supervisor task (readonly=true → PermFetch only)"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: git-gateway-readonly-push-denied
title: Git Gateway Readonly Push Denied
behavior: supervisor
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
e2e_assert_contains "$task_json" '"source":"readonly-push"'
e2e_assert_contains "$task_json" '"push_denied":true'
e2e_assert_contains "$task_json" '"fetch_ok":true'

# The push must not have actually landed — upstream tip stays at baseline.
upstream_tip="$(/usr/bin/git -C "$UPSTREAM_BARE" rev-parse main)"
e2e_log "upstream main tip after hook: $upstream_tip"
[[ "$upstream_tip" == "$baseline_tip" ]] || e2e_fail "upstream main advanced despite readonly denial (baseline=$baseline_tip now=$upstream_tip) — gateway 403 did not actually block the push"

e2e_log "verified: readonly job's push was denied by the gateway while fetch stayed allowed"
