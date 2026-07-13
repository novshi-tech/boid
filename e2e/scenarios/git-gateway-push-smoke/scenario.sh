#!/usr/bin/env bash
set -euo pipefail

# docs/plans/git-gateway-cutover.md PR7b — 新規シナリオ 1/5:
# 「clone → commit → push が gateway 経由で通る」smoke。
#
# 期待する経路: (1) executor task (writable) を dispatch すると、sandbox
# 内 clone (workspace 親化リファクタリング以降の /workspace/<name>) の
# origin remote が gateway URL に設定される。(2) hook 内で新規 commit を
# 作って `git push origin HEAD` すると、gateway が job token を検証して
# 対応する repo permission (PermFetchPush) を確認し、上流の fixture
# upstream bare repo に転送する。(3) upstream bare repo に commit が
# 反映される。
#
# アサーション:
#   - task が done で終わる
#   - hook が payload_patch に pushed_commit を書いている
#   - fixture upstream bare repo の main tip が pushed_commit と一致する
#
# fixture upstream サーバは e2e/run.sh の e2e_setup_fixture_upstream が
# シナリオ workspace 配下の project 一つずつに対して bare repo を作って
# 起動する (skip-e2e-upstream marker がここには無いので自動有効)。
# bare repo は $E2E_ROOT/upstream-repos/${E2E_FIXTURE_UPSTREAM_OWNER}/${name}.git
# の物理 path に落ちる (e2e/lib/common.sh の repo_names 参照)。

APP_DIR="$E2E_WORKSPACE_DIR/app"
UPSTREAM_BARE="$E2E_ROOT/upstream-repos/e2e-fixture/app.git"

[[ -d "$UPSTREAM_BARE" ]] || e2e_fail "fixture upstream bare repo not found: $UPSTREAM_BARE"

# Baseline: fixture upstream has exactly the harness-seeded commit on main
# before the hook runs. `git -C bare.git rev-parse main` is served straight
# from the bare repo on disk — no network involved (bypasses the fake host
# git shim by running the real git via its absolute path, same trick
# e2e/lib/common.sh's seeding uses).
baseline_tip="$(/usr/bin/git -C "$UPSTREAM_BARE" rev-parse main)"
e2e_log "upstream main baseline tip: $baseline_tip"

WS_SLUG="ws-gateway-push"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - git-gateway-push-smoke
YAML

e2e_log "registering project from $APP_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$APP_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "git-gateway-push-smoke" "$WS_SLUG"

e2e_log "creating executor task (writable → PermFetchPush)"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: git-gateway-push-smoke
title: Git Gateway Push Smoke
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
e2e_assert_contains "$task_json" '"source":"gateway-push"'
e2e_assert_contains "$task_json" '"pushed_branch":"main"'

# Extract the commit hash the hook reported and verify it landed on the
# upstream bare repo's main. We use grep+sed instead of jq because the
# e2e harness does not depend on jq being available (see other scenarios).
pushed_commit="$(printf '%s' "$task_json" | grep -oE '"pushed_commit":"[0-9a-f]+"' | head -1 | sed 's/.*:"\([0-9a-f]*\)".*/\1/')"
[[ -n "$pushed_commit" ]] || e2e_fail "hook did not report pushed_commit in artifact"
[[ "$pushed_commit" != "$baseline_tip" ]] || e2e_fail "pushed_commit ($pushed_commit) is same as baseline — hook did not actually add a commit"
e2e_log "hook reported pushed_commit=$pushed_commit"

upstream_tip="$(/usr/bin/git -C "$UPSTREAM_BARE" rev-parse main)"
e2e_log "upstream main tip after hook: $upstream_tip"
[[ "$upstream_tip" == "$pushed_commit" ]] || e2e_fail "upstream main did not advance to pushed_commit (got=$upstream_tip want=$pushed_commit)"

# Extra: the pushed commit must chain from baseline_tip — otherwise the
# hook accidentally rewrote history rather than adding on top of it, which
# would still leave the tip pointing somewhere new but is not the semantic
# we want from a "push a new commit" smoke test.
parent="$(/usr/bin/git -C "$UPSTREAM_BARE" rev-parse "${pushed_commit}^" 2>/dev/null || printf '')"
[[ "$parent" == "$baseline_tip" ]] || e2e_fail "pushed commit's parent ($parent) is not the baseline tip ($baseline_tip) — history was not a plain fast-forward"

e2e_log "verified: gateway push landed on upstream main as a fast-forward"
