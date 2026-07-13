#!/usr/bin/env bash
set -euo pipefail

# docs/plans/git-gateway-cutover.md PR7b — worktree-lifecycle シナリオ
# 書き直し (skip 中 2 本のうちの片方)。
#
# 旧 worktree-lifecycle は退役済み host-side `git worktree add`/`remove`
# を fake-git.log で pin していたため、 sandbox-internal clone モデル
# 下では意味を持たなくなっていた (skip ファイル参照)。書き直しの狙いは
# 「子タスクが `boid/<id8>` branch に着地する」経路 (BuildCloneDeclaration
# の 非-CheckoutOnly 経路 + runner の resolveCloneBranch) を end-to-end
# で pin すること。
#
# 経路:
#   1. parent task (root, executor 的) の hook が (a) parent-marker.txt を
#      commit + push、 (b) 子タスク (behavior: child, auto_start: true,
#      parent_id: $BOID_TASK_ID) を作る。
#   2. auto_start で child が dispatch される。 dispatch 時点で parent の
#      commit は既に upstream に到達済み。
#   3. child の fresh clone は parent の commit を取り込んだ状態から
#      `boid/<id8>` を切る (Branch=ComputeHeadBranch(child), ForkPoint=
#      ComputeForkPoint(parent) = "main", CheckoutOnly=false)。
#   4. child の hook が HEAD ブランチ名 / parent-marker.txt / log を検証。

APP_DIR="$E2E_WORKSPACE_DIR/app"
UPSTREAM_BARE="$E2E_ROOT/upstream-repos/e2e-fixture/app.git"

[[ -d "$UPSTREAM_BARE" ]] || e2e_fail "fixture upstream bare repo not found: $UPSTREAM_BARE"

baseline_tip="$(/usr/bin/git -C "$UPSTREAM_BARE" rev-parse main)"
e2e_log "upstream main baseline tip: $baseline_tip"

WS_SLUG="ws-child-branch"
mkdir -p "$XDG_CONFIG_HOME/boid/workspaces"
cat > "$XDG_CONFIG_HOME/boid/workspaces/${WS_SLUG}.yaml" <<YAML
kits:
  - git-gateway-child-branch
YAML

e2e_log "registering project from $APP_DIR"
e2e_run "$E2E_BIN_DIR/boid" project add "$APP_DIR"
e2e_run "$E2E_BIN_DIR/boid" workspace assign "git-gateway-child-branch" "$WS_SLUG"

e2e_log "creating parent task"
parent_output="$("$E2E_BIN_DIR/boid" task create <<'YAML'
project_id: git-gateway-child-branch
title: Parent Task
behavior: parent
YAML
)"
printf '%s\n' "$parent_output"
parent_id="$(printf '%s\n' "$parent_output" | sed -n 's/^task created: \([0-9a-f-]*\) (.*/\1/p')"
[[ -n "$parent_id" ]] || e2e_fail "failed to parse parent task id"

e2e_log "starting parent task $parent_id"
e2e_run "$E2E_BIN_DIR/boid" action send --task "$parent_id" --type start

e2e_log "waiting for parent completion (parent hook runs 1 job + spawns child)"
parent_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 30s --interval 100ms "$parent_id" done)"
printf '%s\n' "$parent_json"
e2e_assert_contains "$parent_json" '"status":"done"'
e2e_assert_contains "$parent_json" '"source":"spawn-child"'

parent_pushed_commit="$(printf '%s' "$parent_json" | grep -oE '"parent_pushed_commit":"[0-9a-f]+"' | head -1 | sed 's/.*:"\([0-9a-f]*\)".*/\1/')"
[[ -n "$parent_pushed_commit" ]] || e2e_fail "parent did not report parent_pushed_commit in artifact"
e2e_log "parent pushed commit: $parent_pushed_commit"

# Verify parent's push actually landed on upstream main before we start
# looking at the child.
upstream_tip_after_parent="$(/usr/bin/git -C "$UPSTREAM_BARE" rev-parse main)"
[[ "$upstream_tip_after_parent" == "$parent_pushed_commit" ]] \
  || e2e_fail "upstream main did not advance to parent's pushed commit (got=$upstream_tip_after_parent want=$parent_pushed_commit)"

e2e_log "resolving child task by title"
child_id=""
for _ in $(seq 1 100); do
  child_id="$("$E2E_BIN_DIR/boid" task list | awk '/Child Task/{print $1; exit}')"
  [[ -n "$child_id" ]] && break
  sleep 0.1
done
[[ -n "$child_id" ]] || e2e_fail "Child Task not found — parent hook did not spawn it"
e2e_log "child task id: $child_id"

e2e_log "waiting for child task completion"
child_json="$("$E2E_BIN_DIR/boid-e2e" wait-task-status --timeout 30s --interval 100ms "$child_id" done)"
printf '%s\n' "$child_json"
e2e_assert_contains "$child_json" '"status":"done"'
e2e_assert_contains "$child_json" '"source":"verify-child-branch"'
e2e_assert_contains "$child_json" '"branch_matches_boid_prefix":true'
e2e_assert_contains "$child_json" '"parent_marker_present":true'
e2e_assert_contains "$child_json" '"parent_commit_in_log":true'

# The child's reported current_branch should be exactly boid/<child id[:8]>.
expected_branch="boid/${child_id:0:8}"
e2e_assert_contains "$child_json" "\"current_branch\":\"${expected_branch}\""

e2e_log "verified: child task landed on ${expected_branch} with parent's push visible"
