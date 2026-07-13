#!/usr/bin/env bash
set -euo pipefail

# PR7b の worktree-lifecycle 書き直し部分の child 側 hook。
# 子 task の sandbox は BuildCloneDeclaration の非-CheckoutOnly 経路で
# 起動されるはず (root タスクでなく、かつ project.worktree=true):
#   - Branch          = "boid/<id8>"  (ComputeHeadBranch(child_task))
#   - ForkPoint       = "main"        (ComputeForkPoint(parent_task) —
#                                       parent が worktree=true root なので
#                                       ComputeHeadBranch(parent) = BaseBranch)
#   - CheckoutOnly    = false         (runner が checkout -B boid/<id8> baseRef)
#
# よってこの hook が観測すべき事実:
#   (a) 現在の HEAD branch 名が "boid/<self task id の先頭 8 文字>" である
#   (b) parent-marker.txt が clone に含まれている
#       (parent の push が origin/main に届いていて、child の clone が
#        それを fetch した上で boid/<id8> を切っていることの proof)
#   (c) HEAD の親コミット (== origin/main tip) の rev が parent の push
#       と一致していれば理想だが、artifact 経由の受渡しは複雑なので
#       (b) のファイル存在で代替する (retired worktree scenario と同じ
#        「観測可能な副作用で pin する」路線)

current_branch="$(git rev-parse --abbrev-ref HEAD)"
expected_prefix="boid/${BOID_TASK_ID:0:8}"

branch_matches_boid_prefix=false
if [[ "$current_branch" == "$expected_prefix" ]]; then
  branch_matches_boid_prefix=true
fi

parent_marker_present=false
if [[ -f parent-marker.txt ]]; then
  parent_marker_present=true
fi

# HEAD is the "boid/<id8>" branch tip; git-log against just HEAD should
# include parent's commit (the branch was cut from origin/main = parent tip).
parent_commit_in_log=false
if git log --format='%H' HEAD | grep -q .; then
  # find the commit that touched parent-marker.txt
  parent_marker_commit="$(git log --format='%H' HEAD -- parent-marker.txt | head -1)"
  if [[ -n "$parent_marker_commit" ]]; then
    parent_commit_in_log=true
  fi
fi

if [[ "$branch_matches_boid_prefix" != "true" ]] \
   || [[ "$parent_marker_present" != "true" ]] \
   || [[ "$parent_commit_in_log" != "true" ]]; then
  printf 'FAIL: child branch resolution did not behave as expected\n' >&2
  printf '  current_branch=%s expected_prefix=%s\n' "$current_branch" "$expected_prefix" >&2
  printf '  branch_matches_boid_prefix=%s parent_marker_present=%s parent_commit_in_log=%s\n' \
    "$branch_matches_boid_prefix" "$parent_marker_present" "$parent_commit_in_log" >&2
  git log --oneline -5 HEAD >&2 || true
  ls -la >&2 || true
  exit 1
fi

mkdir -p "$HOME/.boid/output"
cat > "$HOME/.boid/output/payload_patch.json" <<EOF
{"payload_patch":{"artifact":{"source":"verify-child-branch","current_branch":"${current_branch}","branch_matches_boid_prefix":true,"parent_marker_present":true,"parent_commit_in_log":true}}}
EOF
