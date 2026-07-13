#!/usr/bin/env bash
set -euo pipefail

# PR7b の worktree-lifecycle 書き直し部分の parent 側 hook。
# 旧 worktree-lifecycle は退役済み host-side `git worktree add`/`remove`
# を fake-git.log で検証するだけだったので、sandbox-internal clone モデル
# 下では意味を失っていた。書き直しの狙いは:
#   - 「子タスクが boid/<id8> branch に着地する」(runner の
#     resolveCloneBranch + BuildCloneDeclaration の非-CheckoutOnly 経路)
#     を end-to-end で pin すること。
#
# 手順:
#   1. parent 自身 (root task) の clone 上で 1 commit push
#      → upstream の main tip が parent's commit まで進む
#   2. `boid task create` で child task (behavior: child, auto_start:
#      true, parent_id: $BOID_TASK_ID) を作る
#      → auto_start でその task が即 dispatch される。 dispatch 時点で
#         parent の commit は既に upstream に到達済み (push を待ってから
#         task create を呼ぶ順)。 child の fresh clone は必ず parent の
#         commit を取り込んだ状態から boid/<id8> を切ることになる。

git config user.name  "Boid E2E Parent"
git config user.email "e2e-parent@boid.test"

printf 'parent marker on main\n' > parent-marker.txt
git add parent-marker.txt
git commit -q -m "parent push before child spawn"
git push -q origin HEAD

parent_pushed_commit="$(git rev-parse HEAD)"

# child が assertion に使うので、parent の pushed_commit を artifact に
# 書き出す。 hook が独自に取得しているので payload merge に頼らない。
# ここでは boid task create の payload 経由でも渡さない — child のクロ
# ーンには parent-marker.txt が確実に fetch されてくるので、その存在を
# 「parent の push が届いた」proof として使う。
boid task create <<EOF
title: Child Task
behavior: child
ref: child-under-parent
parent_id: $BOID_TASK_ID
auto_start: true
EOF

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<EOF
{"payload_patch":{"artifact":{"source":"spawn-child","parent_pushed_commit":"${parent_pushed_commit}"}}}
EOF
