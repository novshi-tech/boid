#!/usr/bin/env bash
set -euo pipefail

# docs/plans/git-gateway-cutover.md PR7b — 新規シナリオ 4/5:
# 「reopen が再 clone + branch checkout になる」。
#
# 経路 (詳細は reopen-reclone.sh のコメント参照):
#   1. executor task を start → 1 個目の hook job が dispatch される
#      → hook が pushed.txt を commit + push、 local-only.txt を残す
#   2. task done を確認 (artifact.attempt == 1)
#   3. reopen action を送る → task が executing に戻る + 2 個目の hook job が
#      dispatch される
#   4. hook は fresh sandbox 内 clone で回るので:
#      - pushed.txt は origin/main に fetch されてきていて visible
#      - local-only.txt は前回の clone を wipe したので absent
#      - HEAD は前回 push した commit と一致
#   5. 全部 hook 側で pin 済み → artifact.attempt == 2 に到達
#
# scenario 側では加えて:
#   - upstream bare repo の main tip が hook 前後 (attempt 2 完了後) も
#     attempt 1 の pushed_commit のままで動かないことを pin

APP_DIR="$E2E_WORKSPACE_DIR/app"
UPSTREAM_BARE="$E2E_ROOT/upstream-repos/e2e-fixture/app.git"

[[ -d "$UPSTREAM_BARE" ]] || e2e_fail "fixture upstream bare repo not found: $UPSTREAM_BARE"

baseline_tip="$(/usr/bin/git -C "$UPSTREAM_BARE" rev-parse main)"
e2e_log "upstream main baseline tip: $baseline_tip"

WS_SLUG="ws-reopen-reclone"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - git-gateway-reopen-reclone
YAML

e2e_log "registering project from $APP_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$APP_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "git-gateway-reopen-reclone" "$WS_SLUG"

e2e_log "creating executor task"
task_create_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: git-gateway-reopen-reclone
title: Git Gateway Reopen Reclone
behavior: executor
YAML
)"
printf '%s\n' "$task_create_output"
task_id="$(printf '%s\n' "$task_create_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$task_id" ]] || e2e_fail "failed to parse task id"

e2e_log "starting task (attempt 1)"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type start
"$E2E_BIN_DIR/boid-e2e" wait-job-count --timeout 15s --interval 100ms "$task_id" 1

task_json_1="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 30s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json_1"
e2e_assert_contains "$task_json_1" '"status":"done"'
e2e_assert_contains "$task_json_1" '"source":"reopen-reclone"'
e2e_assert_contains "$task_json_1" '"attempt":1'

pushed_commit_1="$(printf '%s' "$task_json_1" | grep -oE '"pushed_commit":"[0-9a-f]+"' | head -1 | sed 's/.*:"\([0-9a-f]*\)".*/\1/')"
[[ -n "$pushed_commit_1" ]] || e2e_fail "attempt 1 did not report pushed_commit"
e2e_log "attempt 1 pushed_commit: $pushed_commit_1"

upstream_tip_1="$(/usr/bin/git -C "$UPSTREAM_BARE" rev-parse main)"
[[ "$upstream_tip_1" == "$pushed_commit_1" ]] || e2e_fail "upstream main did not advance to attempt 1 pushed_commit (got=$upstream_tip_1)"

e2e_log "sending reopen action"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$task_id" --type reopen

# wait-job-count is monotonic (jobs never go down), so waiting for count 2
# is race-free — it guarantees attempt 2 has been dispatched before we
# start polling for the second done.
"$E2E_BIN_DIR/boid-e2e" wait-job-count --timeout 15s --interval 100ms "$task_id" 2

task_json_2="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 30s --interval 100ms "$task_id" done)"
printf '%s\n' "$task_json_2"
e2e_assert_contains "$task_json_2" '"status":"done"'
e2e_assert_contains "$task_json_2" '"attempt":2'
e2e_assert_contains "$task_json_2" '"pushed_file_present":true'
e2e_assert_contains "$task_json_2" '"local_only_absent":true'
e2e_assert_contains "$task_json_2" '"head_matches":true'
e2e_assert_contains "$task_json_2" "\"prev_pushed_commit\":\"${pushed_commit_1}\""

# attempt 2 must not have side-effected upstream — the hook only inspects
# the fresh clone.
upstream_tip_2="$(/usr/bin/git -C "$UPSTREAM_BARE" rev-parse main)"
[[ "$upstream_tip_2" == "$pushed_commit_1" ]] || e2e_fail "upstream main moved during attempt 2 (baseline_after_1=$pushed_commit_1 now=$upstream_tip_2)"

e2e_log "verified: reopen re-cloned; pushed commit survived, local-only was wiped, HEAD restored"
